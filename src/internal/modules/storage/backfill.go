package storage

import (
	"encoding/json"
	"fmt"

	"antelope/internal/modules/log"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// BackfillPersonalConfigs copies every legacy per-user row into the new
// storage_configs table as a personal-scoped config.
//
// Before groups existed, a user's storage was a single row keyed by their id.
// Those users must keep working across the cutover — job submission refuses to
// run without storage, so losing a config mid-flight would break every pipeline
// that user owns.
//
// The legacy table is read, never written, and deliberately not dropped: until
// the new path has been exercised in anger, it is the rollback.
//
// Backfilled configs are classified "internal", the cap for personal storage.
// Nothing about a stored connection string says whether it holds controlled
// data, so raising it is an explicit administrative act via the promote flow.
//
// Idempotent. Runs under the same startup lock as the other migrations.
func BackfillPersonalConfigs(db *gorm.DB) error {
	const (
		personalOwnerType     = OwnerTypePersonal
		defaultClassification = "internal"
	)
	if db == nil {
		return nil
	}

	if !db.Migrator().HasTable(&legacyStorageConfigRecord{}) {
		return nil
	}

	var legacy []legacyStorageConfigRecord
	if err := db.Find(&legacy).Error; err != nil {
		return fmt.Errorf("storage: read legacy configs: %w", err)
	}
	if len(legacy) == 0 {
		return nil
	}

	migrated, skipped := 0, 0
	for _, old := range legacy {
		var existing int64
		if err := db.Model(&StorageConfig{}).
			Where("owner_type = ? AND owner_id = ?", personalOwnerType, old.UserID).
			Count(&existing).Error; err != nil {
			return fmt.Errorf("storage: check existing config for user %d: %w", old.UserID, err)
		}
		if existing > 0 {
			skipped++
			continue
		}

		// The payload is copied verbatim, still sealed. Re-encrypting would
		// need the plaintext, and there is no reason to unwrap a secret just to
		// move which row it lives in.
		rec := StorageConfig{
			Name:           "Personal",
			OwnerType:      personalOwnerType,
			OwnerID:        old.UserID,
			Classification: defaultClassification,
			CreatedBy:      old.UserID,
			Type:           old.Type,
			Hash:           old.Hash,
			Encrypted:      old.Encrypted,
			Payload:        old.Payload,
			Endpoint:       endpointFromLegacy(old),
		}
		if err := db.Create(&rec).Error; err != nil {
			return fmt.Errorf("storage: backfill config for user %d: %w", old.UserID, err)
		}
		migrated++
	}

	if migrated > 0 || skipped > 0 {
		log.L().Info("backfilled personal storage configs from legacy table",
			zap.Int("migrated", migrated),
			zap.Int("alreadyPresent", skipped))
	}
	return nil
}

// endpointFromLegacy recovers the non-secret host:port for a legacy row.
//
// Only possible for rows stored without encryption; an encrypted payload needs
// a key this function does not have. Returning "" is safe — it means duplicate
// detection falls back to hash matching for that row, and the endpoint is
// filled in the next time the config is saved.
func endpointFromLegacy(rec legacyStorageConfigRecord) string {
	if rec.Encrypted {
		return ""
	}
	var cfg ProviderConfig
	if err := json.Unmarshal([]byte(rec.Payload), &cfg); err != nil {
		return ""
	}
	return EndpointOf(cfg.Type, cfg.RawConfig)
}

// BackfillEndpoints fills in the endpoint column for configs that lack it,
// decrypting through the manager's key. Separate from BackfillPersonalConfigs
// because it needs a live ClientManager rather than just a DB handle.
//
// Without this, configs migrated from encrypted legacy rows would be invisible
// to endpoint-based duplicate detection — and a shared PHI bucket that goes
// undetected is one that keeps an unclassified personal route into it.
func (m *ClientManager) BackfillEndpoints() error {
	if m.db == nil {
		return nil
	}

	var recs []StorageConfig
	if err := m.db.Where("endpoint IS NULL OR endpoint = ''").Find(&recs).Error; err != nil {
		return fmt.Errorf("storage: scan configs missing endpoint: %w", err)
	}

	filled := 0
	for i := range recs {
		cfg, err := m.openPayload(&recs[i])
		if err != nil {
			log.L().Warn("could not open storage payload to derive endpoint",
				zap.Uint("configID", uint(recs[i].ID)), zap.Error(err))
			continue
		}
		endpoint := EndpointOf(cfg.Type, cfg.RawConfig)
		if endpoint == "" {
			continue
		}
		if err := m.db.Model(&StorageConfig{}).Where("id = ?", recs[i].ID).
			Update("endpoint", endpoint).Error; err != nil {
			log.L().Warn("could not persist derived endpoint",
				zap.Uint("configID", uint(recs[i].ID)), zap.Error(err))
			continue
		}
		filled++
	}

	if filled > 0 {
		log.L().Info("derived endpoints for existing storage configs", zap.Int("count", filled))
	}
	return nil
}
