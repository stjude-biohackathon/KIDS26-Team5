// Package models provides domain model definitions.
// This file contains the bootstrap helpers (Migrate, Seed) that were
// previously embedded in app/db.go and app/init.go.
//
// By moving them here the app package no longer needs to import models,
// breaking the circular dependency risk and keeping infrastructure separate
// from domain concerns.
package models

import (
	"errors"
	"strings"

	"antelope/internal/modules/log"
	"antelope/internal/modules/misc"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Migrate runs AutoMigrate for all domain models.
// Pass this to app.WithMigration in main.
//
//	app.NewApp(cfg, app.WithMigration(models.Migrate))
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&User{},
		&JobTemplate{},
		&Pipeline{},
		&Job{},
		&AuthProvider{},
		&Notification{},
		&MCPConfig{},
		&AgentSkill{},
		&AgentWorkspaceConfig{},
		&APIKey{},
		&Group{},
		&GroupMembership{},
		&StorageGrant{},
		&StorageAuditEvent{},
	); err != nil {
		return err
	}
	return enforceLiveUniqueness(db)
}

// enforceLiveUniqueness scopes uniqueness on the soft-deleting group tables to
// rows that are not deleted.
//
// Group, GroupMembership and StorageGrant all carry gorm.DeletedAt, so deleting
// leaves the row in place as a tombstone. A unique index spanning every row
// counts those tombstones, which makes the deleted value permanently
// unclaimable: removing a member and adding them back, revoking a grant and
// re-granting the same pair, or deleting a group and recreating it under the
// same name all fail on a constraint violation with no way to recover through
// the API.
//
// The invariant that actually holds is "at most one live row", so it is
// expressed as a partial index. Tombstones stay behind as the audit trail of
// what was revoked and when, which is worth keeping for access-control tables.
//
// Drops the older unqualified indexes by name so existing databases converge.
// StorageConfig also soft-deletes but is deliberately absent: it carries no
// unique index at all, and its duplicate detection runs in application code
// under GORM's default scope, so a tombstone there blocks nothing.
func enforceLiveUniqueness(db *gorm.DB) error {
	stmts := []string{
		`DROP INDEX IF EXISTS idx_group_user`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_group_user_live
			ON group_memberships (group_id, user_id) WHERE deleted_at IS NULL`,
		`DROP INDEX IF EXISTS idx_config_group`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_config_group_live
			ON storage_grants (storage_config_id, group_id) WHERE deleted_at IS NULL`,
		`DROP INDEX IF EXISTS idx_groups_name`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_groups_name_live
			ON groups (name) WHERE deleted_at IS NULL`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

// SeedConfig carries the runtime values needed by Seed.
type SeedConfig struct {
	SuperUser         string
	SuperUserPassword string
}

// Seed initialises the bootstrap super-user.
// Pass a closure to app.WithSeed in main:
//
//	app.WithSeed(func(db *gorm.DB, r redis.UniversalClient, sm *storage.ClientManager) {
//	    models.Seed(db, models.SeedConfig{
//	        SuperUser:         cfg.System.SuperUser,
//	        SuperUserPassword: cfg.System.SuperUserPassword,
//	    })
//	})
func Seed(db *gorm.DB, cfg SeedConfig) {
	initSuperUser(db, cfg.SuperUser, cfg.SuperUserPassword)
	seedGroupsFromUsers(db)
}

// ── private helpers ────────────────────────────────────────────────────────

// initSuperUser seeds the single bootstrap admin account. The configured
// email is created with the "super" role when absent, or promoted to "super"
// when it already exists with a lesser role.
func initSuperUser(db *gorm.DB, superUser, password string) {
	emailAddr := strings.TrimSpace(superUser)
	if emailAddr == "" {
		log.L().Info("no super user configured")
		return
	}

	if password == "" {
		log.L().Error("super user password is empty; refusing to seed super user")
		return
	}

	var user User
	result := db.Where("email = ?", emailAddr).First(&user)

	switch result.Error {
	case gorm.ErrRecordNotFound:
		newUser := User{
			Name:       strings.Split(emailAddr, "@")[0],
			Email:      emailAddr,
			Password:   misc.BcryptHash(password),
			Role:       "super",
			Status:     1,
			AuthSource: AuthSourceLocal,
		}
		if err := db.Create(&newUser).Error; err != nil {
			log.L().Error("failed to create super user", zap.String("email", emailAddr), zap.Error(err))
			return
		}
		log.L().Info("created super user", zap.String("email", emailAddr))

	case nil:
		if user.Role != "super" {
			if err := db.Model(&user).Update("role", "super").Error; err != nil {
				log.L().Error("failed to update user to super role", zap.String("email", emailAddr), zap.Error(err))
				return
			}
			log.L().Info("updated user to super role", zap.String("email", emailAddr))
		}

	default:
		log.L().Error("failed to query user", zap.String("email", emailAddr), zap.Error(result.Error))
	}
}

// seedGroupsFromUsers turns the legacy free-text users."group" label into real
// Group rows and memberships. That column was display-only metadata that no
// authorization check ever read; this gives existing deployments a populated
// group structure on first boot instead of an empty one an admin must rebuild
// by hand.
//
// Seeded groups are deliberately conservative: default "internal" clearance and
// personal storage left enabled. Raising a group to restricted/PHI is an
// explicit administrative act, never inferred.
//
// Idempotent — runs on every boot and only fills gaps.
func seedGroupsFromUsers(db *gorm.DB) {
	var users []User
	// "group" is a reserved word in SQL and must stay quoted.
	if err := db.Where(`"group" <> ''`).Find(&users).Error; err != nil {
		log.L().Error("failed to read users for group seeding", zap.Error(err))
		return
	}

	created, joined := 0, 0
	for _, u := range users {
		label := strings.TrimSpace(u.Group)
		if label == "" {
			continue
		}

		var group Group
		err := db.Where("name = ?", groupSlug(label)).First(&group).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			group = Group{
				Name:                 groupSlug(label),
				DisplayName:          label,
				Description:          "Seeded from the legacy user group field",
				Clearance:            ClassificationInternal,
				AllowPersonalStorage: true,
				Source:               GroupSourceLocal,
			}
			if createErr := db.Create(&group).Error; createErr != nil {
				log.L().Error("failed to seed group", zap.String("group", label), zap.Error(createErr))
				continue
			}
			created++
		case err != nil:
			log.L().Error("failed to query group", zap.String("group", label), zap.Error(err))
			continue
		}

		membership := GroupMembership{
			GroupID:     group.ID,
			UserID:      u.ID,
			RoleInGroup: GroupRoleMember,
			Source:      GroupSourceLocal,
		}
		res := db.Where("group_id = ? AND user_id = ?", group.ID, u.ID).
			FirstOrCreate(&membership)
		if res.Error != nil {
			log.L().Error("failed to seed group membership",
				zap.String("group", label), zap.Uint("userID", u.ID), zap.Error(res.Error))
			continue
		}
		if res.RowsAffected > 0 {
			joined++
		}
	}

	if created > 0 || joined > 0 {
		log.L().Info("seeded groups from legacy user group field",
			zap.Int("groupsCreated", created), zap.Int("membershipsCreated", joined))
	}
}

// groupSlug normalises a free-text label into a stable identifier usable as a
// policy subject. Policy subjects are string-matched, so whitespace and case
// drift would silently create parallel groups.
func groupSlug(label string) string {
	slug := strings.ToLower(strings.TrimSpace(label))
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, slug)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	return strings.Trim(slug, "-")
}
