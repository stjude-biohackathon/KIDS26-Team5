package authz_test

// Integration coverage for the group/grant authorization path, run against a
// real Postgres because the behaviour under test is largely SQL: the policy is
// projected from joins across four tables, and a sqlite or mock stand-in would
// verify a different query plan than production runs.
//
// Skipped unless ANTELOPE_TEST_DSN is set, so the normal `go test ./...` stays
// hermetic:
//
//	ANTELOPE_TEST_DSN="host=127.0.0.1 port=5432 user=antelope password=antelope dbname=antelope_rbac_test sslmode=disable" \
//	    go test ./services/authz/ -run Integration -v

import (
	"context"
	"fmt"
	"os"
	"testing"

	nixstorage "antelope/internal/modules/storage"
	"antelope/models"
	"antelope/services/authz"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Shared with services/group: both packages reset the same database, and
// `go test` runs packages in parallel, so they coordinate on this lock rather
// than truncating tables out from under each other. The key must match there.
const integrationDBLock = int64(0x616e7465)

func TestMain(m *testing.M) {
	dsn := os.Getenv("ANTELOPE_TEST_DSN")
	if dsn == "" {
		os.Exit(m.Run())
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration lock: connect: %v\n", err)
		os.Exit(1)
	}
	sqlDB, err := db.DB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration lock: pool: %v\n", err)
		os.Exit(1)
	}

	// A dedicated connection, because an advisory lock is held by the session
	// that took it and the pool would otherwise hand it to someone else.
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration lock: conn: %v\n", err)
		os.Exit(1)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", integrationDBLock); err != nil {
		fmt.Fprintf(os.Stderr, "integration lock: acquire: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	// os.Exit skips defers, so release explicitly before leaving.
	_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", integrationDBLock)
	_ = conn.Close()
	os.Exit(code)
}

func testDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("ANTELOPE_TEST_DSN")
	if dsn == "" {
		t.Skip("ANTELOPE_TEST_DSN not set; skipping database integration test")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	if err := models.Migrate(db); err != nil {
		t.Fatalf("migrate domain models: %v", err)
	}
	if err := nixstorage.MigrateSchema(db); err != nil {
		t.Fatalf("migrate storage schema: %v", err)
	}
	return db
}

// reset clears the tables this test owns. Hard deletes, because the models use
// soft deletion and a previous run's tombstones would otherwise accumulate.
func reset(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range []string{"storage_grants", "group_memberships", "groups", "storage_configs", "users"} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
}

func mkUser(t *testing.T, db *gorm.DB, name, role string) models.User {
	t.Helper()
	u := models.User{Name: name, Email: name + "@example.test", Role: role, Status: 1}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return u
}

func mkGroup(t *testing.T, db *gorm.DB, name string, clearance models.Classification) models.Group {
	t.Helper()
	g := models.Group{
		Name:                 name,
		DisplayName:          name,
		Clearance:            clearance,
		AllowPersonalStorage: true,
		Source:               models.GroupSourceLocal,
	}
	if err := db.Create(&g).Error; err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	return g
}

func mkConfig(t *testing.T, db *gorm.DB, name, ownerType string, ownerID uint, classification string) nixstorage.StorageConfig {
	t.Helper()
	c := nixstorage.StorageConfig{
		Name:           name,
		OwnerType:      ownerType,
		OwnerID:        ownerID,
		Classification: classification,
		Type:           string(nixstorage.ProviderMinio),
		Hash:           "hash-" + name,
		Payload:        `{"type":"minio","config":{},"hash":"x"}`,
		Endpoint:       name + ".example.test:9000",
	}
	if err := db.Create(&c).Error; err != nil {
		t.Fatalf("create config %s: %v", name, err)
	}
	return c
}

func join(t *testing.T, db *gorm.DB, groupID, userID uint) {
	t.Helper()
	m := models.GroupMembership{GroupID: groupID, UserID: userID, RoleInGroup: models.GroupRoleMember}
	if err := db.Create(&m).Error; err != nil {
		t.Fatalf("join group: %v", err)
	}
}

func grant(t *testing.T, db *gorm.DB, configID nixstorage.ConfigID, groupID uint, level string) {
	t.Helper()
	g := models.StorageGrant{StorageConfigID: uint(configID), GroupID: groupID, AccessLevel: level}
	if err := db.Create(&g).Error; err != nil {
		t.Fatalf("grant: %v", err)
	}
}

func TestIntegrationGroupGrantsReachMembers(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	member := mkUser(t, db, "member", "user")
	outsider := mkUser(t, db, "outsider", "user")
	lab := mkGroup(t, db, "smith-lab", models.ClassificationRestricted)
	shared := mkConfig(t, db, "lab-bucket", nixstorage.OwnerTypeGroup, lab.ID, "restricted")

	join(t, db, lab.ID, member.ID)
	grant(t, db, shared.ID, lab.ID, models.AccessWrite)

	a, err := authz.New(db, nil)
	if err != nil {
		t.Fatalf("build authorizer: %v", err)
	}

	res := authz.StorageResource(uint(shared.ID))

	// A write grant must imply read; that implication is expanded at projection
	// time rather than by a matcher, so it is worth asserting directly.
	for _, action := range []authz.Action{authz.ActionRead, authz.ActionWrite} {
		ok, err := a.Can(ctx, member.ID, action, res)
		if err != nil {
			t.Fatalf("Can(%s): %v", action, err)
		}
		if !ok {
			t.Errorf("member should be allowed to %s group storage", action)
		}
	}

	// Write must NOT imply admin: a member with write on shared lab storage
	// should not be able to delete the bucket holding everyone's results.
	if ok, _ := a.Can(ctx, member.ID, authz.ActionAdmin, res); ok {
		t.Error("write grant must not confer admin")
	}

	if ok, _ := a.Can(ctx, outsider.ID, authz.ActionRead, res); ok {
		t.Error("non-member must not reach group storage")
	}
}

func TestIntegrationRevocationTakesEffect(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	member := mkUser(t, db, "member", "user")
	lab := mkGroup(t, db, "lab", models.ClassificationInternal)
	cfg := mkConfig(t, db, "bucket", nixstorage.OwnerTypeGroup, lab.ID, "internal")
	join(t, db, lab.ID, member.ID)
	grant(t, db, cfg.ID, lab.ID, models.AccessRead)

	a, _ := authz.New(db, nil)
	res := authz.StorageResource(uint(cfg.ID))

	if ok, _ := a.Can(ctx, member.ID, authz.ActionRead, res); !ok {
		t.Fatal("precondition: member should start with access")
	}

	// Removing the membership must end access after the policy is rebuilt from
	// the domain tables — this is the revocation path, so a stale allow here
	// would be a security hole rather than a cosmetic bug.
	if err := db.Exec("DELETE FROM group_memberships WHERE group_id = ? AND user_id = ?", lab.ID, member.ID).Error; err != nil {
		t.Fatalf("remove membership: %v", err)
	}
	if err := a.ReloadPolicy(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if ok, _ := a.Can(ctx, member.ID, authz.ActionRead, res); ok {
		t.Error("removed member must lose access immediately")
	}
}

func TestIntegrationPersonalConfigIsSelfGranted(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	owner := mkUser(t, db, "owner", "user")
	other := mkUser(t, db, "other", "user")
	cfg := mkConfig(t, db, "mine", nixstorage.OwnerTypePersonal, owner.ID, "internal")

	a, _ := authz.New(db, nil)
	res := authz.StorageResource(uint(cfg.ID))

	// Personal storage is modelled as an implicit self-grant so there is one
	// path through Can() rather than a special case.
	if ok, _ := a.Can(ctx, owner.ID, authz.ActionAdmin, res); !ok {
		t.Error("owner should have admin over their own storage")
	}
	if ok, _ := a.Can(ctx, other.ID, authz.ActionRead, res); ok {
		t.Error("personal storage must not be readable by anyone else")
	}
}

func TestIntegrationExplainNamesWhoToAsk(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	outsider := mkUser(t, db, "outsider", "user")
	pi := mkUser(t, db, "pi", "user")
	lab := mkGroup(t, db, "smith-lab", models.ClassificationInternal)
	lab.DisplayName = "Smith Lab"
	db.Save(&lab)

	cfg := mkConfig(t, db, "lab-bucket", nixstorage.OwnerTypeGroup, lab.ID, "internal")
	grant(t, db, cfg.ID, lab.ID, models.AccessWrite)

	owner := models.GroupMembership{GroupID: lab.ID, UserID: pi.ID, RoleInGroup: models.GroupRoleOwner}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatalf("add owner: %v", err)
	}

	a, _ := authz.New(db, nil)
	decision, err := a.Explain(ctx, outsider.ID, authz.ActionRead, authz.StorageResource(uint(cfg.ID)))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if decision.Allowed {
		t.Fatal("outsider should be denied")
	}
	if decision.Reason != authz.DenyNoGrant {
		t.Errorf("reason = %q, want %q", decision.Reason, authz.DenyNoGrant)
	}

	// The denial has to name someone who can actually fix it, otherwise the
	// user's only recourse is a support ticket.
	if len(decision.RequestAccessFrom) != 1 {
		t.Fatalf("expected one group to request access from, got %d", len(decision.RequestAccessFrom))
	}
	ref := decision.RequestAccessFrom[0]
	if ref.DisplayName != "Smith Lab" {
		t.Errorf("group display name = %q, want %q", ref.DisplayName, "Smith Lab")
	}
	if len(ref.OwnerEmails) != 1 || ref.OwnerEmails[0] != pi.Email {
		t.Errorf("owner emails = %v, want [%s]", ref.OwnerEmails, pi.Email)
	}
}

func TestIntegrationInsufficientLevelIsDistinguished(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	member := mkUser(t, db, "member", "user")
	lab := mkGroup(t, db, "lab", models.ClassificationInternal)
	cfg := mkConfig(t, db, "bucket", nixstorage.OwnerTypeGroup, lab.ID, "internal")
	join(t, db, lab.ID, member.ID)
	grant(t, db, cfg.ID, lab.ID, models.AccessRead)

	a, _ := authz.New(db, nil)
	decision, err := a.Explain(ctx, member.ID, authz.ActionWrite, authz.StorageResource(uint(cfg.ID)))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// "You can see it but not write" leads somewhere different from "you cannot
	// see it at all", so the two must not collapse into one message.
	if decision.Allowed {
		t.Fatal("read-only member should not be allowed to write")
	}
	if decision.Reason != authz.DenyInsufficientLevel {
		t.Errorf("reason = %q, want %q", decision.Reason, authz.DenyInsufficientLevel)
	}
}

func TestIntegrationAccessibleStorageIDs(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	user := mkUser(t, db, "user", "user")
	lab := mkGroup(t, db, "lab", models.ClassificationInternal)
	join(t, db, lab.ID, user.ID)

	personal := mkConfig(t, db, "personal", nixstorage.OwnerTypePersonal, user.ID, "internal")
	groupRW := mkConfig(t, db, "group-rw", nixstorage.OwnerTypeGroup, lab.ID, "internal")
	groupRO := mkConfig(t, db, "group-ro", nixstorage.OwnerTypeGroup, lab.ID, "internal")
	unrelated := mkConfig(t, db, "unrelated", nixstorage.OwnerTypeGroup, lab.ID+999, "internal")

	grant(t, db, groupRW.ID, lab.ID, models.AccessWrite)
	grant(t, db, groupRO.ID, lab.ID, models.AccessRead)

	a, _ := authz.New(db, nil)

	readable, err := a.AccessibleStorageIDs(ctx, user.ID, authz.ActionRead)
	if err != nil {
		t.Fatalf("AccessibleStorageIDs: %v", err)
	}
	assertSet(t, "readable", readable, personal.ID, groupRW.ID, groupRO.ID)

	writable, err := a.AccessibleStorageIDs(ctx, user.ID, authz.ActionWrite)
	if err != nil {
		t.Fatalf("AccessibleStorageIDs: %v", err)
	}
	assertSet(t, "writable", writable, personal.ID, groupRW.ID)

	if contains(readable, unrelated.ID) {
		t.Error("storage belonging to an unrelated group must not be listed")
	}
}

// A config that stays owner_type=personal and is granted to a group, reached by
// a different user who is only a member of that group.
//
// Every other grant test here starts from a group-owned config, but the path an
// admin actually takes is to share a bucket they already configured for
// themselves — so ownership stays personal and only a grant appears. That
// combination reaches Can() through the group rule while the self-grant rule
// for a different subject also exists for the same object, and nothing else
// covered it.
func TestIntegrationPersonalConfigGrantedToGroupReachesMembers(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	cfgOwner := mkUser(t, db, "cfgowner", "user") // registered the config
	member := mkUser(t, db, "labmember", "user")  // in the lab, owns nothing
	outsider := mkUser(t, db, "otherlab", "user") // in a different lab

	lab := mkGroup(t, db, "smith-lab", models.ClassificationInternal)
	elsewhere := mkGroup(t, db, "gottlieb-lab", models.ClassificationInternal)

	// Ownership deliberately stays personal; only the grant is added.
	cfg := mkConfig(t, db, "shared-bucket", nixstorage.OwnerTypePersonal, cfgOwner.ID, "internal")

	join(t, db, lab.ID, member.ID)
	join(t, db, elsewhere.ID, outsider.ID)
	grant(t, db, cfg.ID, lab.ID, models.AccessRead)

	a, err := authz.New(db, nil)
	if err != nil {
		t.Fatalf("build authorizer: %v", err)
	}
	res := authz.StorageResource(uint(cfg.ID))

	if ok, err := a.Can(ctx, member.ID, authz.ActionRead, res); err != nil {
		t.Fatalf("Can(read): %v", err)
	} else if !ok {
		t.Error("group member must reach a personal config granted to their group")
	}

	// The listing path is what the storage screens render, so a config that
	// passes Can but never appears in this list is still invisible to the user.
	readable, err := a.AccessibleStorageIDs(ctx, member.ID, authz.ActionRead)
	if err != nil {
		t.Fatalf("AccessibleStorageIDs: %v", err)
	}
	if !contains(readable, cfg.ID) {
		t.Errorf("granted personal config missing from member's readable set %v", readable)
	}

	// A read grant must not become write just because the object is owned
	// personally by someone holding admin over it.
	if ok, _ := a.Can(ctx, member.ID, authz.ActionWrite, res); ok {
		t.Error("read grant must not confer write on a personally owned config")
	}

	// The owner keeps full control through the self-grant.
	if ok, _ := a.Can(ctx, cfgOwner.ID, authz.ActionAdmin, res); !ok {
		t.Error("owner should retain admin over their own config after granting it")
	}

	// Membership in some other group must not leak access.
	if ok, _ := a.Can(ctx, outsider.ID, authz.ActionRead, res); ok {
		t.Error("member of an unrelated group must not reach the config")
	}
	outsiderReadable, err := a.AccessibleStorageIDs(ctx, outsider.ID, authz.ActionRead)
	if err != nil {
		t.Fatalf("AccessibleStorageIDs(outsider): %v", err)
	}
	if contains(outsiderReadable, cfg.ID) {
		t.Errorf("config leaked into an outsider's readable set %v", outsiderReadable)
	}
}

func TestIntegrationSuperUserBypasses(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	super := mkUser(t, db, "root", "super")
	someone := mkUser(t, db, "someone", "user")
	cfg := mkConfig(t, db, "private", nixstorage.OwnerTypePersonal, someone.ID, "internal")

	a, _ := authz.New(db, nil)
	decision, err := a.Explain(ctx, super.ID, authz.ActionAdmin, authz.StorageResource(uint(cfg.ID)))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !decision.Allowed || !decision.SuperBypass {
		t.Errorf("super user should be allowed with SuperBypass set, got %+v", decision)
	}
}

func TestIntegrationBackfillMigratesLegacyConfigs(t *testing.T) {
	db := testDB(t)
	reset(t, db)

	user := mkUser(t, db, "legacy", "user")

	payload := `{"type":"minio","config":{"host":"old.example.test","port":9000},"hash":"legacyhash"}`
	err := db.Exec(
		`INSERT INTO infra_storage_configs (user_id, type, hash, encrypted, payload, updated_at)
		 VALUES (?, ?, ?, ?, ?, NOW())`,
		user.ID, "minio", "legacyhash", false, payload).Error
	if err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	defer db.Exec("DELETE FROM infra_storage_configs WHERE user_id = ?", user.ID)

	if err := nixstorage.BackfillPersonalConfigs(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var got nixstorage.StorageConfig
	err = db.Where("owner_type = ? AND owner_id = ?", nixstorage.OwnerTypePersonal, user.ID).First(&got).Error
	if err != nil {
		t.Fatalf("backfilled config not found: %v", err)
	}

	if got.Classification != "internal" {
		t.Errorf("classification = %q, want internal (the personal cap)", got.Classification)
	}
	if got.Payload != payload {
		t.Error("payload must be copied verbatim so credentials are not re-entered")
	}
	// Unencrypted legacy rows can have their endpoint recovered, which is what
	// makes them visible to duplicate detection.
	if got.Endpoint != "old.example.test:9000" {
		t.Errorf("endpoint = %q, want old.example.test:9000", got.Endpoint)
	}

	// Running again must not duplicate the row.
	if err := nixstorage.BackfillPersonalConfigs(db); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	var count int64
	db.Model(&nixstorage.StorageConfig{}).
		Where("owner_type = ? AND owner_id = ?", nixstorage.OwnerTypePersonal, user.ID).
		Count(&count)
	if count != 1 {
		t.Errorf("backfill is not idempotent: %d rows after two runs", count)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func contains(ids []nixstorage.ConfigID, want nixstorage.ConfigID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func assertSet(t *testing.T, label string, got []nixstorage.ConfigID, want ...nixstorage.ConfigID) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d ids %v, want %d %v", label, len(got), got, len(want), want)
		return
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("%s: missing config %d (got %v)", label, w, got)
		}
	}
}
