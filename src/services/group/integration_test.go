package group_test

// Integration coverage for changing an existing grant's access level in place.
//
// Run against a real Postgres, because the behaviour under test is the upsert
// on (storage_config_id, group_id) together with the partial unique index that
// scopes it to live rows — neither of which a mock would exercise.
//
// Skipped unless ANTELOPE_TEST_DSN is set:
//
//	ANTELOPE_TEST_DSN="host=127.0.0.1 port=5432 user=antelope password=antelope dbname=antelope_rbac_test sslmode=disable" \
//	    go test ./services/group/ -run Integration -v

import (
	"context"
	"fmt"
	"os"
	"testing"

	nixstorage "antelope/internal/modules/storage"
	"antelope/models"
	"antelope/services/authz"
	groupsvc "antelope/services/group"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The integration packages share one database and reset() truncates tables
// wholesale, but `go test` runs packages in parallel — so without coordination
// one package wipes rows another is mid-test on, surfacing as foreign key
// violations that read like authorization bugs. A Postgres advisory lock makes
// the packages take turns without anyone having to remember `-p 1`.
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

func reset(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range []string{"storage_grants", "group_memberships", "groups", "storage_configs", "users"} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
}

// newService wires the real group service. The client manager is a global
// singleton, so it is initialised once for the whole test binary; only its
// database handle matters here, since grant checks read config rows and never
// open a connection to an object store.
func newService(t *testing.T, db *gorm.DB) groupsvc.Service {
	t.Helper()
	nixstorage.InitGlobalManager(nil, db, nil, nixstorage.NewMinioProvider())
	a, err := authz.New(db, nil)
	if err != nil {
		t.Fatalf("build authorizer: %v", err)
	}
	return groupsvc.NewService(db, nixstorage.GetGlobalManager(), a)
}

func mkConfig(t *testing.T, db *gorm.DB, name string, ownerID uint, classification string) nixstorage.StorageConfig {
	t.Helper()
	c := nixstorage.StorageConfig{
		Name:           name,
		OwnerType:      nixstorage.OwnerTypePersonal,
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

// mkDuplicate creates a personal config pointing at a given backend. Promotion
// keys off hash and endpoint rather than name, so callers pass those directly
// to build the "everyone configured the same bucket themselves" shape that
// promotion exists to clean up.
func mkDuplicate(t *testing.T, db *gorm.DB, name string, ownerID uint, hash, endpoint string) nixstorage.StorageConfig {
	t.Helper()
	c := nixstorage.StorageConfig{
		Name:           name,
		OwnerType:      nixstorage.OwnerTypePersonal,
		OwnerID:        ownerID,
		Classification: "internal",
		Type:           string(nixstorage.ProviderMinio),
		Hash:           hash,
		Payload:        `{"type":"minio","config":{},"hash":"x"}`,
		Endpoint:       endpoint,
	}
	if err := db.Create(&c).Error; err != nil {
		t.Fatalf("create config %s: %v", name, err)
	}
	return c
}

func liveGrants(t *testing.T, db *gorm.DB, configID nixstorage.ConfigID, groupID uint) []models.StorageGrant {
	t.Helper()
	var grants []models.StorageGrant
	err := db.Where("storage_config_id = ? AND group_id = ?", configID, groupID).Find(&grants).Error
	if err != nil {
		t.Fatalf("read grants: %v", err)
	}
	return grants
}

// Raising and lowering an existing grant must edit the row rather than stack a
// second one. Before this was reachable from the UI, admins had to revoke and
// re-grant, which left a tombstone behind for every level change.
func TestIntegrationGrantLevelChangesInPlace(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	svc := newService(t, db)

	admin := models.User{Name: "admin", Email: "admin@example.test", Role: "super", Status: 1}
	member := models.User{Name: "member", Email: "member@example.test", Role: "user", Status: 1}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if err := db.Create(&member).Error; err != nil {
		t.Fatalf("create member: %v", err)
	}

	lab := models.Group{
		Name: "smith-lab", DisplayName: "Smith Lab",
		Clearance: models.ClassificationInternal, AllowPersonalStorage: true,
		Source: models.GroupSourceLocal,
	}
	if err := db.Create(&lab).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := db.Create(&models.GroupMembership{
		GroupID: lab.ID, UserID: member.ID, RoleInGroup: models.GroupRoleMember,
	}).Error; err != nil {
		t.Fatalf("join group: %v", err)
	}

	cfg := mkConfig(t, db, "shared", admin.ID, "internal")
	in := groupsvc.GrantInput{StorageConfigID: cfg.ID, GroupID: lab.ID, AccessLevel: models.AccessRead}

	if err := svc.GrantStorage(ctx, admin.ID, in); err != nil {
		t.Fatalf("initial grant: %v", err)
	}

	a, _ := authz.New(db, nil)
	res := authz.StorageResource(uint(cfg.ID))
	if ok, _ := a.Can(ctx, member.ID, authz.ActionWrite, res); ok {
		t.Fatal("precondition: a read grant must not confer write")
	}

	// Upgrade.
	in.AccessLevel = models.AccessWrite
	if err := svc.GrantStorage(ctx, admin.ID, in); err != nil {
		t.Fatalf("raise to write: %v", err)
	}

	grants := liveGrants(t, db, cfg.ID, lab.ID)
	if len(grants) != 1 {
		t.Fatalf("expected exactly one live grant after the change, got %d", len(grants))
	}
	if grants[0].AccessLevel != models.AccessWrite {
		t.Errorf("access level = %q, want write", grants[0].AccessLevel)
	}

	// The level change has to reach members, which depends on the policy
	// being rebuilt — a stale allow or deny here is the whole point of the
	// feature failing.
	a2, _ := authz.New(db, nil)
	if ok, _ := a2.Can(ctx, member.ID, authz.ActionWrite, res); !ok {
		t.Error("member should gain write immediately after the grant is raised")
	}

	// Downgrade must work the same way, and must actually remove capability.
	in.AccessLevel = models.AccessRead
	if err := svc.GrantStorage(ctx, admin.ID, in); err != nil {
		t.Fatalf("lower to read: %v", err)
	}

	grants = liveGrants(t, db, cfg.ID, lab.ID)
	if len(grants) != 1 {
		t.Fatalf("expected one live grant after downgrade, got %d", len(grants))
	}
	if grants[0].AccessLevel != models.AccessRead {
		t.Errorf("access level = %q, want read", grants[0].AccessLevel)
	}

	a3, _ := authz.New(db, nil)
	if ok, _ := a3.Can(ctx, member.ID, authz.ActionWrite, res); ok {
		t.Error("member must lose write when the grant is lowered to read")
	}
	if ok, _ := a3.Can(ctx, member.ID, authz.ActionRead, res); !ok {
		t.Error("member should still hold read after the downgrade")
	}
}

// Editing an existing grant must not be a way around clearance. The check runs
// before the upsert, so it applies to updates exactly as it does to new grants.
func TestIntegrationGrantLevelChangeStillHonoursClearance(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	svc := newService(t, db)

	admin := models.User{Name: "admin", Email: "admin@example.test", Role: "super", Status: 1}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatalf("create admin: %v", err)
	}

	lab := models.Group{
		Name: "lab", DisplayName: "Lab",
		Clearance: models.ClassificationRestricted, AllowPersonalStorage: true,
		Source: models.GroupSourceLocal,
	}
	if err := db.Create(&lab).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}

	cfg := mkConfig(t, db, "sensitive", admin.ID, string(models.ClassificationRestricted))
	in := groupsvc.GrantInput{StorageConfigID: cfg.ID, GroupID: lab.ID, AccessLevel: models.AccessRead}
	if err := svc.GrantStorage(ctx, admin.ID, in); err != nil {
		t.Fatalf("initial grant: %v", err)
	}

	// Lower the group's clearance underneath the existing grant, then try to
	// raise the level: the group is no longer cleared for this data, so the
	// edit must be refused rather than waved through as "already granted".
	if err := db.Model(&models.Group{}).Where("id = ?", lab.ID).
		Update("clearance", models.ClassificationInternal).Error; err != nil {
		t.Fatalf("lower clearance: %v", err)
	}

	in.AccessLevel = models.AccessWrite
	if err := svc.GrantStorage(ctx, admin.ID, in); err == nil {
		t.Error("raising a grant above the group's clearance must be refused")
	}

	grants := liveGrants(t, db, cfg.ID, lab.ID)
	if len(grants) != 1 || grants[0].AccessLevel != models.AccessRead {
		t.Errorf("refused edit must leave the grant untouched, got %+v", grants)
	}
}

// Promotion's whole purpose is to collapse a bucket that several people
// configured individually into one group-owned config. The dangerous half of
// that is what happens to someone outside the group who could reach the bucket
// through their own copy: their copy is retired, so their access must actually
// end rather than linger on a config nobody is looking at any more.
func TestIntegrationPromotionStripsAccessFromOutsiders(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	svc := newService(t, db)

	admin := models.User{Name: "admin", Email: "admin@example.test", Role: "super", Status: 1}
	bob := models.User{Name: "bob", Email: "bob@example.test", Role: "user", Status: 1}
	carol := models.User{Name: "carol", Email: "carol@example.test", Role: "user", Status: 1}
	for _, u := range []*models.User{&admin, &bob, &carol} {
		if err := db.Create(u).Error; err != nil {
			t.Fatalf("create user %s: %v", u.Name, err)
		}
	}

	lab := models.Group{
		Name: "smith-lab", DisplayName: "Smith Lab",
		Clearance: models.ClassificationInternal, AllowPersonalStorage: true,
		Source: models.GroupSourceLocal,
	}
	if err := db.Create(&lab).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}
	// Bob is inside the group, Carol is not. That asymmetry is the test.
	if err := db.Create(&models.GroupMembership{
		GroupID: lab.ID, UserID: bob.ID, RoleInGroup: models.GroupRoleMember,
	}).Error; err != nil {
		t.Fatalf("join group: %v", err)
	}

	const hash, endpoint = "shared-bucket-hash", "lab-bucket.example.test:9000"
	bobCfg := mkDuplicate(t, db, "bob's copy", bob.ID, hash, endpoint)
	carolCfg := mkDuplicate(t, db, "carol's copy", carol.ID, hash, endpoint)

	// Precondition: each of them reaches the bucket today through their own
	// personal config. If this does not hold the test proves nothing.
	pre, err := authz.New(db, nil)
	if err != nil {
		t.Fatalf("build authorizer: %v", err)
	}
	if ok, _ := pre.Can(ctx, carol.ID, authz.ActionRead, authz.StorageResource(uint(carolCfg.ID))); !ok {
		t.Fatal("precondition: carol should reach the bucket through her own config")
	}

	in := groupsvc.PromoteInput{
		StorageConfigID: bobCfg.ID,
		GroupID:         lab.ID,
		Classification:  "internal",
		Name:            "Smith Lab bucket",
		AccessLevel:     models.AccessWrite,
	}

	// The preview must name Carol before anything is committed; an admin who
	// cannot see who is about to be cut off cannot make this call safely.
	preview, err := svc.PreviewPromotion(ctx, in)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(preview.DuplicatesRetired) != 1 || preview.DuplicatesRetired[0].ID != uint(carolCfg.ID) {
		t.Errorf("preview should retire carol's copy, got %+v", preview.DuplicatesRetired)
	}
	if len(preview.LosingAccess) != 1 || preview.LosingAccess[0].OwnerMail != carol.Email {
		t.Errorf("preview should warn that carol loses access, got %+v", preview.LosingAccess)
	}

	result, err := svc.PromoteConfig(ctx, admin.ID, in)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if result.DuplicatesRetired != 1 {
		t.Errorf("duplicates retired = %d, want 1", result.DuplicatesRetired)
	}

	var promoted nixstorage.StorageConfig
	if err := db.First(&promoted, bobCfg.ID).Error; err != nil {
		t.Fatalf("reload promoted config: %v", err)
	}
	if promoted.OwnerType != nixstorage.OwnerTypeGroup || promoted.OwnerID != lab.ID {
		t.Errorf("config should be group-owned, got %s/%d", promoted.OwnerType, promoted.OwnerID)
	}

	grants := liveGrants(t, db, bobCfg.ID, lab.ID)
	if len(grants) != 1 || grants[0].AccessLevel != models.AccessWrite {
		t.Errorf("promotion should leave one write grant, got %+v", grants)
	}

	// Carol's copy must be gone, not merely ignored.
	var retired nixstorage.StorageConfig
	if err := db.Unscoped().First(&retired, carolCfg.ID).Error; err != nil {
		t.Fatalf("reload carol's config: %v", err)
	}
	if !retired.DeletedAt.Valid {
		t.Error("carol's duplicate should be soft-deleted by the promotion")
	}

	after, err := authz.New(db, nil)
	if err != nil {
		t.Fatalf("rebuild authorizer: %v", err)
	}

	// Bob keeps working, now through the group rather than his own config.
	if ok, _ := after.Can(ctx, bob.ID, authz.ActionWrite, authz.StorageResource(uint(bobCfg.ID))); !ok {
		t.Error("bob should still reach the bucket through the group grant")
	}

	// Carol is cut off on both routes: the promoted config she was never
	// granted, and the retired copy that used to be hers.
	if ok, _ := after.Can(ctx, carol.ID, authz.ActionRead, authz.StorageResource(uint(bobCfg.ID))); ok {
		t.Error("carol is not in the group and must not reach the promoted config")
	}
	if ok, _ := after.Can(ctx, carol.ID, authz.ActionRead, authz.StorageResource(uint(carolCfg.ID))); ok {
		t.Error("carol's retired duplicate must not keep granting access")
	}

	ids, err := after.AccessibleStorageIDs(ctx, carol.ID, authz.ActionRead)
	if err != nil {
		t.Fatalf("accessible ids: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("carol should have no reachable storage after promotion, got %v", ids)
	}
}

// The config chosen for promotion is itself somebody's personal config, and
// the drawer defaults to the first in the cluster — so the person promoting is
// quite likely to pick one belonging to a non-member. That owner loses their
// private route to the bucket exactly like the duplicate owners do, so leaving
// them out of the warning makes the warning wrong in the one case an admin
// cannot spot by eye.
func TestIntegrationPromotionWarnsAboutTheOwnerItStrips(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	svc := newService(t, db)

	admin := models.User{Name: "admin", Email: "admin@example.test", Role: "super", Status: 1}
	bob := models.User{Name: "bob", Email: "bob@example.test", Role: "user", Status: 1}
	carol := models.User{Name: "carol", Email: "carol@example.test", Role: "user", Status: 1}
	for _, u := range []*models.User{&admin, &bob, &carol} {
		if err := db.Create(u).Error; err != nil {
			t.Fatalf("create user %s: %v", u.Name, err)
		}
	}

	lab := models.Group{
		Name: "gottlieb-lab", DisplayName: "Gottlieb Lab",
		Clearance: models.ClassificationPHI, AllowPersonalStorage: true,
		Source: models.GroupSourceLocal,
	}
	if err := db.Create(&lab).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}
	// Carol is in the group; Bob, whose config gets promoted, is not.
	if err := db.Create(&models.GroupMembership{
		GroupID: lab.ID, UserID: carol.ID, RoleInGroup: models.GroupRoleMember,
	}).Error; err != nil {
		t.Fatalf("join group: %v", err)
	}

	const hash, endpoint = "lab-hash", "lab-bucket.example.test:9000"
	bobCfg := mkDuplicate(t, db, "bob's copy", bob.ID, hash, endpoint)
	mkDuplicate(t, db, "carol's copy", carol.ID, hash, endpoint)

	in := groupsvc.PromoteInput{
		StorageConfigID: bobCfg.ID,
		GroupID:         lab.ID,
		Classification:  "internal",
		AccessLevel:     models.AccessWrite,
	}

	preview, err := svc.PreviewPromotion(ctx, in)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	var warned bool
	for _, u := range preview.LosingAccess {
		if u.OwnerMail == bob.Email {
			warned = true
		}
	}
	if !warned {
		t.Errorf("preview must warn that bob loses access; got %+v", preview.LosingAccess)
	}

	// And the warning has to be true: confirm he really is cut off.
	if _, err := svc.PromoteConfig(ctx, admin.ID, in); err != nil {
		t.Fatalf("promote: %v", err)
	}
	after, err := authz.New(db, nil)
	if err != nil {
		t.Fatalf("rebuild authorizer: %v", err)
	}
	if ok, _ := after.Can(ctx, bob.ID, authz.ActionRead, authz.StorageResource(uint(bobCfg.ID))); ok {
		t.Error("bob is not in the group, so promoting his config must end his access")
	}
}

// Promotion sets the classification on a bucket, so it is a way to label data
// as controlled. A group that is not cleared for that label must not receive
// it, and a refusal must leave the config exactly as it was — a half-applied
// promotion would mark the bucket restricted while the personal copies, capped
// at "internal", still pointed straight at it.
func TestIntegrationPromotionAboveClearanceIsRefusedAtomically(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	svc := newService(t, db)

	admin := models.User{Name: "admin", Email: "admin@example.test", Role: "super", Status: 1}
	bob := models.User{Name: "bob", Email: "bob@example.test", Role: "user", Status: 1}
	carol := models.User{Name: "carol", Email: "carol@example.test", Role: "user", Status: 1}
	for _, u := range []*models.User{&admin, &bob, &carol} {
		if err := db.Create(u).Error; err != nil {
			t.Fatalf("create user %s: %v", u.Name, err)
		}
	}

	lab := models.Group{
		Name: "open-lab", DisplayName: "Open Lab",
		Clearance: models.ClassificationInternal, AllowPersonalStorage: true,
		Source: models.GroupSourceLocal,
	}
	if err := db.Create(&lab).Error; err != nil {
		t.Fatalf("create group: %v", err)
	}

	const hash, endpoint = "phi-bucket-hash", "phi-bucket.example.test:9000"
	bobCfg := mkDuplicate(t, db, "bob's copy", bob.ID, hash, endpoint)
	carolCfg := mkDuplicate(t, db, "carol's copy", carol.ID, hash, endpoint)

	_, err := svc.PromoteConfig(ctx, admin.ID, groupsvc.PromoteInput{
		StorageConfigID: bobCfg.ID,
		GroupID:         lab.ID,
		Classification:  string(models.ClassificationRestricted),
		AccessLevel:     models.AccessWrite,
	})
	if err == nil {
		t.Fatal("promoting restricted data into an internal-cleared group must be refused")
	}

	var cfg nixstorage.StorageConfig
	if err := db.First(&cfg, bobCfg.ID).Error; err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.OwnerType != nixstorage.OwnerTypePersonal || cfg.OwnerID != bob.ID {
		t.Errorf("refused promotion must leave the config personal, got %s/%d", cfg.OwnerType, cfg.OwnerID)
	}
	if cfg.Classification != "internal" {
		t.Errorf("refused promotion must not raise classification, got %q", cfg.Classification)
	}
	if got := liveGrants(t, db, bobCfg.ID, lab.ID); len(got) != 0 {
		t.Errorf("refused promotion must not create a grant, got %+v", got)
	}

	// The duplicate must survive too, otherwise a failed promotion would still
	// have cut Carol off from the bucket.
	var dup nixstorage.StorageConfig
	if err := db.First(&dup, carolCfg.ID).Error; err != nil {
		t.Errorf("refused promotion must not retire the duplicate: %v", err)
	}
	_ = dup
}
