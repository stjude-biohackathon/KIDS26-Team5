package audit_test

// Integration coverage for the storage audit trail, run against a real
// Postgres: the point of the table is durability and filtered retrieval, and
// an in-memory stand-in would test neither.
//
// Skipped unless ANTELOPE_TEST_DSN is set:
//
//	ANTELOPE_TEST_DSN="host=127.0.0.1 port=5432 user=antelope password=antelope dbname=antelope_rbac_test sslmode=disable" \
//	    go test ./services/audit/ -run Integration -v
//
// Use -p 1 when running this alongside the authz and group integration
// packages: they share one database and each truncates the user table, so
// running the packages concurrently makes them clobber each other.

import (
	"context"
	"os"
	"testing"
	"time"

	"antelope/models"
	"antelope/services/audit"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

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
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// reset clears this test's tables. The group tables come first because they
// hold foreign keys into users, which other integration packages populate
// against the same database.
func reset(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range []string{
		"storage_audit_events", "storage_grants", "group_memberships", "groups", "users",
	} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
}

func mkUser(t *testing.T, db *gorm.DB, name string) models.User {
	t.Helper()
	u := models.User{Name: name, Email: name + "@example.test", Role: "user", Status: 1}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestIntegrationRecordsAllowedActionWithGroupAttribution(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	user := mkUser(t, db, "alice")
	groupID := uint(7)

	r := audit.NewRecorder(db)
	r.RecordStorage(ctx, audit.Event{
		ActorID:         user.ID,
		Action:          models.StorageActionListObjects,
		StorageConfigID: 1,
		Bucket:          "lab-data",
		ObjectKey:       "runs/2026/",
		Outcome:         models.AuditOutcomeAllowed,
		ViaGroupID:      &groupID,
		ViaGroupName:    "Smith Lab",
	})

	events, total, err := r.ListStorage(ctx, audit.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(events) != 1 {
		t.Fatalf("expected one event, got total=%d len=%d", total, len(events))
	}

	got := events[0]
	if got.ActorID != user.ID {
		t.Errorf("actor id = %d, want %d", got.ActorID, user.ID)
	}
	// The email is denormalized at write time; a join would lose it the moment
	// the user is deleted.
	if got.ActorEmail != user.Email {
		t.Errorf("actor email = %q, want %q", got.ActorEmail, user.Email)
	}
	if got.Bucket != "lab-data" || got.ObjectKey != "runs/2026/" {
		t.Errorf("bucket/key not recorded: %+v", got)
	}
	// The group attribution is the RBAC-relevant fact: it answers why this was
	// permitted, not merely who did it.
	if got.ViaGroupID == nil || *got.ViaGroupID != groupID {
		t.Errorf("via group id = %v, want %d", got.ViaGroupID, groupID)
	}
	if got.ViaGroupName != "Smith Lab" {
		t.Errorf("via group name = %q, want Smith Lab", got.ViaGroupName)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at must be set")
	}
}

// A denial is usually the more security-relevant event, so it has to be
// recorded with the reason rather than dropped.
func TestIntegrationRecordsDenialWithReason(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	carol := mkUser(t, db, "carol")
	r := audit.NewRecorder(db)

	r.RecordStorage(ctx, audit.Event{
		ActorID:         carol.ID,
		Action:          models.StorageActionListBuckets,
		StorageConfigID: 1,
		Outcome:         models.AuditOutcomeDenied,
		DenyReason:      "no_grant",
	})

	events, _, err := r.ListStorage(ctx, audit.Filter{Outcome: models.AuditOutcomeDenied})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one denial, got %d", len(events))
	}
	if events[0].DenyReason != "no_grant" {
		t.Errorf("deny reason = %q, want no_grant", events[0].DenyReason)
	}
	if events[0].ViaGroupID != nil {
		t.Error("a denial has no conferring group")
	}
}

// Users are hard-deleted, so attribution has to survive the account going
// away — otherwise the trail goes blank exactly when someone has left.
func TestIntegrationAttributionSurvivesUserDeletion(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	user := mkUser(t, db, "departing")
	r := audit.NewRecorder(db)
	r.RecordStorage(ctx, audit.Event{
		ActorID:         user.ID,
		Action:          models.StorageActionDeleteObject,
		StorageConfigID: 1,
		Bucket:          "lab-data",
		ObjectKey:       "results.bam",
		Outcome:         models.AuditOutcomeAllowed,
	})

	if err := db.Exec("DELETE FROM users WHERE id = ?", user.ID).Error; err != nil {
		t.Fatalf("delete user: %v", err)
	}

	events, _, err := r.ListStorage(ctx, audit.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected the event to outlive the user, got %d", len(events))
	}
	if events[0].ActorEmail != "departing@example.test" {
		t.Errorf("actor email = %q, want the denormalized address", events[0].ActorEmail)
	}
}

func TestIntegrationListFiltersAndBoundsPageSize(t *testing.T) {
	db := testDB(t)
	reset(t, db)
	ctx := context.Background()

	alice := mkUser(t, db, "alice")
	bob := mkUser(t, db, "bob")
	r := audit.NewRecorder(db)

	for i := 0; i < 3; i++ {
		r.RecordStorage(ctx, audit.Event{
			ActorID: alice.ID, Action: models.StorageActionListBuckets,
			StorageConfigID: 1, Outcome: models.AuditOutcomeAllowed,
		})
	}
	r.RecordStorage(ctx, audit.Event{
		ActorID: bob.ID, Action: models.StorageActionListBuckets,
		StorageConfigID: 2, Outcome: models.AuditOutcomeAllowed,
	})

	_, total, err := r.ListStorage(ctx, audit.Filter{ActorID: alice.ID})
	if err != nil {
		t.Fatalf("filter by user: %v", err)
	}
	if total != 3 {
		t.Errorf("alice's events = %d, want 3", total)
	}

	_, total, err = r.ListStorage(ctx, audit.Filter{StorageConfigID: 2})
	if err != nil {
		t.Fatalf("filter by config: %v", err)
	}
	if total != 1 {
		t.Errorf("config 2 events = %d, want 1", total)
	}

	future := time.Now().Add(time.Hour)
	_, total, err = r.ListStorage(ctx, audit.Filter{From: &future})
	if err != nil {
		t.Fatalf("filter by time: %v", err)
	}
	if total != 0 {
		t.Errorf("events after now+1h = %d, want 0", total)
	}

	// An oversized page size must be clamped rather than honoured; the table
	// grows with every browse, so this is the difference between a slow query
	// and an outage.
	events, _, err := r.ListStorage(ctx, audit.Filter{PageSize: 100000})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) > audit.MaxPageSize {
		t.Errorf("returned %d rows, above the %d cap", len(events), audit.MaxPageSize)
	}
}

// The audit path must never be able to fail a user's storage request. A
// recorder pointed at a database with no table stands in for the table being
// unavailable; the call must return normally rather than panic or block.
func TestIntegrationRecordFailureIsNotFatal(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	broken := db.Session(&gorm.Session{}).Table("storage_audit_events_missing")
	r := audit.NewRecorder(broken)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RecordStorage(ctx, audit.Event{
			ActorID: 1, Action: models.StorageActionListBuckets,
			StorageConfigID: 1, Outcome: models.AuditOutcomeAllowed,
		})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RecordStorage blocked instead of failing quietly")
	}
}
