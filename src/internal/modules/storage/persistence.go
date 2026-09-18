package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// StorageConfig is the durable record of one storage backend.
//
// It replaces the original one-row-per-user table. A config is now a resource
// in its own right: it has an owner (a user for personal storage, a group for
// shared storage), a sensitivity classification, and — for group-owned
// configs — grants that decide who may use it. Credentials live here and are
// never returned to the people who use them.
//
// Payload holds the ProviderConfig JSON, encrypted with AES-256-GCM when an
// encryption key is configured (Encrypted = true); otherwise plaintext JSON.
//
// Defined inside this module rather than the business models package so the
// module keeps its layering, mirroring the nomad infra-model pattern. The
// domain vocabulary for OwnerType and Classification lives in models.
type StorageConfig struct {
	ID ConfigID `gorm:"primaryKey" json:"id"`

	Name string `gorm:"type:varchar(128);not null" json:"name"`

	// OwnerType is "personal" or "group"; OwnerID is the user or group id.
	OwnerType string `gorm:"type:varchar(16);not null;index:idx_storage_owner,priority:1" json:"owner_type"`
	OwnerID   uint   `gorm:"not null;index:idx_storage_owner,priority:2" json:"owner_id"`

	// Classification is asserted by an administrator; it cannot be inferred
	// from a connection string.
	Classification string `gorm:"type:varchar(20);not null;default:'internal'" json:"classification"`

	CreatedBy uint `gorm:"comment:'user who registered this config'" json:"created_by,omitempty"`

	Type      string `gorm:"type:varchar(32);not null" json:"type"`
	Hash      string `gorm:"type:varchar(128);not null;index" json:"-"`
	Encrypted bool   `gorm:"not null" json:"-"`
	Payload   string `gorm:"type:text;not null" json:"-"`

	// Endpoint is the non-secret "host:port" of the backend, stored in the
	// clear specifically so duplicate detection can run in SQL. Hash covers the
	// whole config including credentials, so it only matches people sharing one
	// credential; two users with separate keys to the same server differ by
	// hash but agree here.
	Endpoint string `gorm:"type:varchar(256);index" json:"endpoint,omitempty"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (StorageConfig) TableName() string { return "storage_configs" }

// legacyStorageConfigRecord is the original per-user table. Retained so the
// backfill can read it and so a rollback remains possible; nothing writes it.
type legacyStorageConfigRecord struct {
	UserID    uint   `gorm:"primaryKey"`
	Type      string `gorm:"type:varchar(32);not null"`
	Hash      string `gorm:"type:varchar(128);not null"`
	Encrypted bool   `gorm:"not null"`
	Payload   string `gorm:"type:text;not null"`
	UpdatedAt time.Time
}

func (legacyStorageConfigRecord) TableName() string { return "infra_storage_configs" }

// Canonical values for StorageConfig.OwnerType. Defined here because this
// module owns the column; models mirrors them as domain vocabulary.
const (
	OwnerTypePersonal = "personal"
	OwnerTypeGroup    = "group"
)

// ConfigSpec describes a config to create. Credentials arrive separately so
// they take the encryption path rather than sitting in a plain struct field.
type ConfigSpec struct {
	Name           string
	OwnerType      string
	OwnerID        uint
	Classification string
	CreatedBy      uint
}

// MigrateSchema creates/updates the storage persistence tables. Call it from
// the coordinated startup migration (under the same Redis lock as the domain
// models) so concurrent pods don't run DDL simultaneously.
func MigrateSchema(db *gorm.DB) error {
	if err := db.AutoMigrate(&StorageConfig{}, &legacyStorageConfigRecord{}); err != nil {
		return fmt.Errorf("storage: migrate config tables: %w", err)
	}
	return nil
}

// ── Config record CRUD ────────────────────────────────────────────────────────

// CreateConfig persists a new config and returns it with its assigned ID.
func (m *ClientManager) CreateConfig(spec ConfigSpec, cfg ProviderConfig, configJSON []byte, endpoint string) (*StorageConfig, error) {
	if m.db == nil {
		return nil, errors.New("storage: no durable store configured")
	}

	payload, encrypted, err := m.sealPayload(configJSON)
	if err != nil {
		return nil, err
	}

	rec := StorageConfig{
		Name:           spec.Name,
		OwnerType:      spec.OwnerType,
		OwnerID:        spec.OwnerID,
		Classification: spec.Classification,
		CreatedBy:      spec.CreatedBy,
		Type:           string(cfg.Type),
		Hash:           cfg.Hash,
		Encrypted:      encrypted,
		Payload:        payload,
		Endpoint:       endpoint,
	}
	if err := m.db.Create(&rec).Error; err != nil {
		return nil, fmt.Errorf("storage: create config: %w", err)
	}
	return &rec, nil
}

// UpdateConfigPayload replaces the credentials on an existing config.
func (m *ClientManager) UpdateConfigPayload(configID ConfigID, cfg ProviderConfig, configJSON []byte, endpoint string) error {
	if m.db == nil {
		return errors.New("storage: no durable store configured")
	}

	payload, encrypted, err := m.sealPayload(configJSON)
	if err != nil {
		return err
	}

	return m.db.Model(&StorageConfig{}).
		Where("id = ?", configID).
		Updates(map[string]any{
			"type":      string(cfg.Type),
			"hash":      cfg.Hash,
			"encrypted": encrypted,
			"payload":   payload,
			"endpoint":  endpoint,
		}).Error
}

// GetConfig returns one config record, or (nil, nil) when absent.
func (m *ClientManager) GetConfig(configID ConfigID) (*StorageConfig, error) {
	if m.db == nil {
		return nil, nil
	}
	var rec StorageConfig
	err := m.db.Where("id = ?", configID).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get config: %w", err)
	}
	return &rec, nil
}

// ListConfigsByIDs returns the named configs, preserving no particular order.
func (m *ClientManager) ListConfigsByIDs(ids []ConfigID) ([]StorageConfig, error) {
	if m.db == nil || len(ids) == 0 {
		return nil, nil
	}
	var recs []StorageConfig
	if err := m.db.Where("id IN ?", ids).Find(&recs).Error; err != nil {
		return nil, fmt.Errorf("storage: list configs: %w", err)
	}
	return recs, nil
}

// ListConfigsByOwner returns every config owned by one user or group.
func (m *ClientManager) ListConfigsByOwner(ownerType string, ownerID uint) ([]StorageConfig, error) {
	if m.db == nil {
		return nil, nil
	}
	var recs []StorageConfig
	if err := m.db.Where("owner_type = ? AND owner_id = ?", ownerType, ownerID).
		Find(&recs).Error; err != nil {
		return nil, fmt.Errorf("storage: list configs by owner: %w", err)
	}
	return recs, nil
}

// PersonalConfigForUser returns a user's own config, or (nil, nil) if they
// have not registered one.
func (m *ClientManager) PersonalConfigForUser(userID uint) (*StorageConfig, error) {
	if m.db == nil {
		return nil, nil
	}
	var rec StorageConfig
	err := m.db.Where("owner_type = ? AND owner_id = ?", OwnerTypePersonal, userID).
		Order("id ASC").First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: personal config lookup: %w", err)
	}
	return &rec, nil
}

// PersonalClientForUser returns the client for a user's own storage.
//
// This is the narrow "my own bucket" path, used where the resource in question
// is unambiguously the caller's: a personal config is an implicit self-grant,
// so no authorization lookup is involved. Anything that may reach group storage
// must resolve a config id through the storage service instead, which consults
// authz.
func (m *ClientManager) PersonalClientForUser(userID uint) StorageClient {
	rec, err := m.PersonalConfigForUser(userID)
	if err != nil || rec == nil {
		return nil
	}
	return m.GetClient(rec.ID)
}

// DeleteConfig soft-deletes a config record and drops its caches.
func (m *ClientManager) DeleteConfig(configID ConfigID) error {
	if m.db == nil {
		return nil
	}
	if err := m.db.Where("id = ?", configID).Delete(&StorageConfig{}).Error; err != nil {
		return fmt.Errorf("storage: delete config: %w", err)
	}
	m.evict(configID)
	return nil
}

// FindConfigsMatching returns configs that point at the same backend as the
// given config: identical hash (same credentials) or the same endpoint
// (same server, possibly different keys). Used both to find shared buckets
// hiding as personal configs and to block registering a personal copy of
// something a group already manages.
func (m *ClientManager) FindConfigsMatching(hash, endpoint string, excludeID ConfigID) ([]StorageConfig, error) {
	if m.db == nil {
		return nil, nil
	}
	var recs []StorageConfig
	q := m.db.Where("id <> ?", excludeID)
	switch {
	case hash != "" && endpoint != "":
		q = q.Where("hash = ? OR endpoint = ?", hash, endpoint)
	case hash != "":
		q = q.Where("hash = ?", hash)
	case endpoint != "":
		q = q.Where("endpoint = ?", endpoint)
	default:
		return nil, nil
	}
	if err := q.Find(&recs).Error; err != nil {
		return nil, fmt.Errorf("storage: find matching configs: %w", err)
	}
	return recs, nil
}

// DuplicateCluster groups personal configs that point at the same backend.
type DuplicateCluster struct {
	Endpoint string          `json:"endpoint"`
	Hash     string          `json:"hash"`
	Configs  []StorageConfig `json:"configs"`
}

// FindPersonalDuplicates returns clusters of personal configs sharing a
// backend. With no group storage available until now, a lab necessarily
// configured the same bucket on every member, so these clusters are the
// worklist of shared storage waiting to be promoted.
func (m *ClientManager) FindPersonalDuplicates() ([]DuplicateCluster, error) {
	if m.db == nil {
		return nil, nil
	}

	var recs []StorageConfig
	if err := m.db.Where("owner_type = ?", OwnerTypePersonal).
		Order("endpoint ASC, hash ASC, id ASC").Find(&recs).Error; err != nil {
		return nil, fmt.Errorf("storage: scan personal configs: %w", err)
	}

	byEndpoint := make(map[string][]StorageConfig)
	for _, r := range recs {
		key := r.Endpoint
		if key == "" {
			key = "hash:" + r.Hash
		}
		byEndpoint[key] = append(byEndpoint[key], r)
	}

	clusters := make([]DuplicateCluster, 0)
	for _, group := range byEndpoint {
		if len(group) < 2 {
			continue
		}
		clusters = append(clusters, DuplicateCluster{
			Endpoint: group[0].Endpoint,
			Hash:     group[0].Hash,
			Configs:  group,
		})
	}
	return clusters, nil
}

// ReparentConfig moves a config to a new owner and classification. Used by the
// promote flow to turn a personal config into group-owned storage.
func (m *ClientManager) ReparentConfig(configID ConfigID, ownerType string, ownerID uint, classification, name string) error {
	if m.db == nil {
		return errors.New("storage: no durable store configured")
	}
	updates := map[string]any{
		"owner_type":     ownerType,
		"owner_id":       ownerID,
		"classification": classification,
	}
	if name != "" {
		updates["name"] = name
	}
	if err := m.db.Model(&StorageConfig{}).Where("id = ?", configID).
		Updates(updates).Error; err != nil {
		return fmt.Errorf("storage: reparent config: %w", err)
	}
	m.evict(configID)
	return nil
}

// ReparentConfigTx and DeleteConfigTx are the transaction-joining forms of the
// methods above, for callers changing configs and domain tables together.
//
// They take the transaction explicitly rather than returning a manager bound to
// it: ClientManager owns a singleflight group and a cache, so copying it to
// swap the DB handle would duplicate a mutex and split the cache in two.
//
// Cache eviction is deliberately left to the caller, after commit. Evicting
// inside the transaction would drop a live client for a change that might still
// roll back.
func (m *ClientManager) ReparentConfigTx(tx *gorm.DB, configID ConfigID, ownerType string, ownerID uint, classification, name string) error {
	updates := map[string]any{
		"owner_type":     ownerType,
		"owner_id":       ownerID,
		"classification": classification,
	}
	if name != "" {
		updates["name"] = name
	}
	if err := tx.Model(&StorageConfig{}).Where("id = ?", configID).
		Updates(updates).Error; err != nil {
		return fmt.Errorf("storage: reparent config: %w", err)
	}
	return nil
}

func (m *ClientManager) DeleteConfigTx(tx *gorm.DB, configID ConfigID) error {
	if err := tx.Where("id = ?", configID).Delete(&StorageConfig{}).Error; err != nil {
		return fmt.Errorf("storage: delete config: %w", err)
	}
	return nil
}

// Evict drops a config from both caches. Call after a transaction that changed
// the config commits.
func (m *ClientManager) Evict(configID ConfigID) { m.evict(configID) }

// ── Payload helpers ───────────────────────────────────────────────────────────

// sealPayload encrypts configJSON when a key is configured.
func (m *ClientManager) sealPayload(configJSON []byte) (payload string, encrypted bool, err error) {
	if m.box == nil {
		return string(configJSON), false, nil
	}
	enc, encErr := m.box.Encrypt(configJSON)
	if encErr != nil {
		return "", false, fmt.Errorf("storage: encrypt config: %w", encErr)
	}
	return enc, true, nil
}

// openPayload reverses sealPayload for a stored record.
func (m *ClientManager) openPayload(rec *StorageConfig) (*ProviderConfig, error) {
	plaintext := []byte(rec.Payload)
	if rec.Encrypted {
		if m.box == nil {
			return nil, errors.New("storage: config is encrypted but no encryption key is configured")
		}
		dec, err := m.box.Decrypt(rec.Payload)
		if err != nil {
			return nil, fmt.Errorf("storage: decrypt config: %w", err)
		}
		plaintext = dec
	}

	var cfg ProviderConfig
	if err := json.Unmarshal(plaintext, &cfg); err != nil {
		return nil, fmt.Errorf("storage: deserialize config: %w", err)
	}
	return &cfg, nil
}

// loadFromDB returns the durable ProviderConfig for configID, or (nil, nil).
func (m *ClientManager) loadFromDB(configID ConfigID) (*ProviderConfig, error) {
	if m.db == nil {
		return nil, nil
	}

	rec, err := m.GetConfig(configID)
	if err != nil || rec == nil {
		return nil, err
	}
	return m.openPayload(rec)
}

// existsInDB reports whether a config record exists.
func (m *ClientManager) existsInDB(configID ConfigID) bool {
	if m.db == nil {
		return false
	}
	var count int64
	if err := m.db.Model(&StorageConfig{}).Where("id = ?", configID).Count(&count).Error; err != nil {
		return false
	}
	return count > 0
}
