package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"antelope/internal/modules/log"
	"antelope/pkg/secretbox"

	gocache "github.com/patrickmn/go-cache"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

const (
	// DefaultCacheTTL is the default TTL for local cache entries.
	DefaultCacheTTL = 5 * time.Minute
	// DefaultCleanupInterval is the default cleanup interval for expired local cache entries.
	DefaultCleanupInterval = 10 * time.Minute
	// redisKeyPrefix is the Redis key prefix for all storage provider configs.
	redisKeyPrefix = "storage:config:"
)

// ── Singleton ─────────────────────────────────────────────────────────────────

var (
	globalManager *ClientManager
	globalOnce    sync.Once
)

// InitGlobalManager initialises the package-level singleton with default TTL
// settings. db is the durable source of truth (Postgres); box, when non-nil,
// encrypts persisted configs at rest. Call once at startup before GetGlobalManager.
func InitGlobalManager(redisClient redis.UniversalClient, db *gorm.DB, box *secretbox.Box, providers ...StorageProvider) {
	globalOnce.Do(func() {
		globalManager = newClientManager(redisClient, db, box, DefaultCacheTTL, DefaultCleanupInterval, providers...)
	})
}

// InitGlobalManagerWithTTL is like InitGlobalManager but lets the caller override TTL settings.
func InitGlobalManagerWithTTL(
	redisClient redis.UniversalClient,
	db *gorm.DB,
	box *secretbox.Box,
	ttl, cleanupInterval time.Duration,
	providers ...StorageProvider,
) {
	globalOnce.Do(func() {
		globalManager = newClientManager(redisClient, db, box, ttl, cleanupInterval, providers...)
	})
}

// GetGlobalManager returns the singleton ClientManager.
// Panics if InitGlobalManager has not been called — this is intentional:
// a missing init is a programming error, not a runtime condition.
func GetGlobalManager() *ClientManager {
	if globalManager == nil {
		panic("storage: global manager not initialised — call InitGlobalManager first")
	}
	return globalManager
}

// ── Internal cache entry ──────────────────────────────────────────────────────

// cachedEntry holds a ready-to-use StorageClient alongside the config hash
// that was used to create it, so we can detect cross-node config changes.
type cachedEntry struct {
	client     StorageClient
	configHash string
}

// ── ClientManager ─────────────────────────────────────────────────────────────

// ClientManager manages StorageClient instances for each user.
// It is provider-agnostic: all backend-specific logic lives in the registered
// StorageProvider implementations.
//
// Call RegisterProvider (or pass providers to the Init functions) to add backends.
// The manager itself never imports minio-go, s3, or any other SDK directly.
type ClientManager struct {
	localCache  *gocache.Cache
	redisClient redis.UniversalClient
	db          *gorm.DB       // durable source of truth (may be nil in minimal setups)
	box         *secretbox.Box // optional at-rest encryption (nil = plaintext)
	cacheTTL    time.Duration
	sfGroup     singleflight.Group

	providersMu sync.RWMutex
	providers   map[ProviderType]StorageProvider
}

// newClientManager is the internal constructor used by both Init variants.
func newClientManager(
	redisClient redis.UniversalClient,
	db *gorm.DB,
	box *secretbox.Box,
	ttl, cleanupInterval time.Duration,
	providers ...StorageProvider,
) *ClientManager {
	m := &ClientManager{
		localCache:  gocache.New(ttl, cleanupInterval),
		redisClient: redisClient,
		db:          db,
		box:         box,
		cacheTTL:    ttl,
		providers:   make(map[ProviderType]StorageProvider),
	}
	for _, p := range providers {
		m.providers[p.Type()] = p
	}
	return m
}

// RegisterProvider adds a new StorageProvider at runtime.
// Safe for concurrent use; overwrites any existing provider of the same type.
func (m *ClientManager) RegisterProvider(p StorageProvider) {
	m.providersMu.Lock()
	defer m.providersMu.Unlock()
	m.providers[p.Type()] = p
}

func (m *ClientManager) provider(t ProviderType) (StorageProvider, error) {
	m.providersMu.RLock()
	defer m.providersMu.RUnlock()
	p, ok := m.providers[t]
	if !ok {
		return nil, fmt.Errorf("storage: unknown provider %q — did you call RegisterProvider", t)
	}
	return p, nil
}

// ── Public API ────────────────────────────────────────────────────────────────

// GetClient returns the StorageClient for configID with lazy loading and
// cross-node config validation via Redis.
//
// Keyed by storage config, not by user: one config may be shared by every
// member of a group, so the client and its cache entry belong to the config.
// Deciding *which* config a user may reach is the caller's job — this manager
// is deliberately ignorant of authorization and will hand back a client for any
// config id it is given.
//
//   - local cache hit → validate hash against Redis → return or reload
//   - cache miss → singleflight → load config → create → cache → return
//   - config load is Redis-first, falling back to the durable Postgres store on a
//     Redis miss (and repopulating the cache); see getConfigFromRedis
//   - Redis unavailable on a cache hit → return cached client as fallback
//   - no config anywhere → return nil
func (m *ClientManager) GetClient(configID ConfigID) StorageClient {
	if m.redisClient == nil {
		log.L().Warn("Redis client not available, cannot get storage client")
		return nil
	}

	cacheKey := localCacheKey(configID)

	// ── L1: local cache ───────────────────────────────────────────────────────
	if cached, found := m.localCache.Get(cacheKey); found {
		entry := cached.(*cachedEntry)

		redisConfig, err := m.getConfigFromRedis(configID)
		if err != nil {
			// DESIGN (intentional, not a bug): fail-open. When Redis is
			// unreachable we cannot revalidate the config hash across pods, so we
			// serve the locally-cached client rather than hard-failing every
			// storage operation. Trade-off: availability over consistency. The
			// only stale window is "config changed on another pod AND Redis is
			// down AND the local entry hasn't expired", bounded by the local cache
			// TTL (m.cacheTTL, default 5m). If instant cross-pod invalidation is
			// ever required even during a Redis outage, add a pub/sub invalidation
			// channel instead of shortening the TTL.
			log.L().Warn("failed to reach Redis, using cached client as fallback",
				zap.Uint("configID", uint(configID)),
				zap.Error(err))
			return entry.client
		}

		if redisConfig == nil {
			// Config was deleted from Redis; evict local cache.
			m.localCache.Delete(cacheKey)
			return nil
		}

		if entry.configHash == redisConfig.Hash {
			return entry.client // fast path
		}

		// Config changed across nodes — fall through to recreate.
		log.L().Info("storage config changed, recreating client",
			zap.Uint("configID", uint(configID)),
			zap.String("providerType", string(redisConfig.Type)))
	}

	// ── L2: Redis (via singleflight to prevent stampede) ──────────────────────
	v, err, _ := m.sfGroup.Do(cacheKey, func() (any, error) {
		// Double-check local cache inside the singleflight — a concurrent
		// goroutine may have already populated it.  We verify the hash here
		// too (unlike the original) to avoid returning a stale entry when the
		// cache-miss was triggered by a config change.
		redisConfig, err := m.getConfigFromRedis(configID)
		if err != nil {
			log.L().Error("failed to load storage config from Redis",
				zap.Uint("configID", uint(configID)),
				zap.Error(err))
			return nil, err
		}
		if redisConfig == nil {
			return nil, nil
		}

		// Use the local cache only if the hash is still current.
		if cached, found := m.localCache.Get(cacheKey); found {
			entry := cached.(*cachedEntry)
			if entry.configHash == redisConfig.Hash {
				return entry.client, nil
			}
		}

		client, err := m.createClientFromConfig(redisConfig)
		if err != nil {
			log.L().Error("failed to create storage client",
				zap.Uint("configID", uint(configID)),
				zap.String("providerType", string(redisConfig.Type)),
				zap.Error(err))
			return nil, err
		}

		m.localCache.Set(cacheKey, &cachedEntry{
			client:     client,
			configHash: redisConfig.Hash,
		}, m.cacheTTL)

		log.L().Debug("created and cached storage client",
			zap.Uint("configID", uint(configID)),
			zap.String("providerType", string(redisConfig.Type)))

		return client, nil
	})

	if err != nil || v == nil {
		return nil
	}
	return v.(StorageClient)
}

// ValidateConfig creates and pings a client without persisting anything,
// returning the validated client alongside the envelope and serialised JSON
// the caller needs to store it.
//
// rawConfig must be a valid JSON encoding of the config struct expected by
// the given providerType (e.g. MinioConfig for ProviderMinio).
func (m *ClientManager) ValidateConfig(providerType ProviderType, rawConfig json.RawMessage) (StorageClient, ProviderConfig, []byte, error) {
	p, err := m.provider(providerType)
	if err != nil {
		return nil, ProviderConfig{}, nil, err
	}

	client, err := p.CreateClient(rawConfig)
	if err != nil {
		return nil, ProviderConfig{}, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		return nil, ProviderConfig{}, nil, fmt.Errorf("storage: connection validation failed: %w", err)
	}

	envelope := ProviderConfig{
		Type:      providerType,
		RawConfig: rawConfig,
		Hash:      p.ComputeHash(rawConfig),
	}

	configJSON, err := json.Marshal(envelope)
	if err != nil {
		return nil, ProviderConfig{}, nil, fmt.Errorf("storage: failed to serialise config: %w", err)
	}
	return client, envelope, configJSON, nil
}

// SetClient validates rawConfig, writes it to the existing config record, and
// refreshes both caches. Use CreateConfig for a config that does not exist yet.
func (m *ClientManager) SetClient(configID ConfigID, providerType ProviderType, rawConfig json.RawMessage) (StorageClient, error) {
	client, envelope, configJSON, err := m.ValidateConfig(providerType, rawConfig)
	if err != nil {
		return nil, err
	}

	endpoint := EndpointOf(providerType, rawConfig)

	// Durable source of truth first (encrypted at rest when a key is configured)
	// so the config survives a Redis flush/eviction.
	if err := m.UpdateConfigPayload(configID, envelope, configJSON, endpoint); err != nil {
		return nil, fmt.Errorf("storage: failed to persist config: %w", err)
	}

	m.primeCaches(configID, client, envelope.Hash, configJSON)

	log.L().Info("saved storage config and created client",
		zap.Uint("configID", uint(configID)),
		zap.String("providerType", string(providerType)))

	return client, nil
}

// primeCaches writes a freshly validated config into Redis and the local cache.
// Redis is a rebuildable cache — best-effort. Postgres remains authoritative.
// The Redis copy is encrypted too (when a key is configured), matching the
// durable one, so secrets are not exposed via snapshots or a memory dump.
func (m *ClientManager) primeCaches(configID ConfigID, client StorageClient, hash string, configJSON []byte) {
	if m.redisClient != nil {
		ctx := context.Background()
		if payload, encErr := m.encodeConfigForCache(configJSON); encErr != nil {
			log.L().Warn("failed to encrypt storage config for Redis cache",
				zap.Uint("configID", uint(configID)), zap.Error(encErr))
		} else if err := m.redisClient.Set(ctx, redisConfigKey(configID), payload, 0).Err(); err != nil {
			log.L().Warn("failed to cache storage config in Redis (Postgres holds the source of truth)",
				zap.Uint("configID", uint(configID)), zap.Error(err))
		}
	}

	m.localCache.Set(localCacheKey(configID), &cachedEntry{
		client:     client,
		configHash: hash,
	}, m.cacheTTL)
}

// RemoveClient deletes the config from the durable store and both caches.
func (m *ClientManager) RemoveClient(configID ConfigID) error {
	// Durable store first — it is authoritative.
	if err := m.DeleteConfig(configID); err != nil {
		return fmt.Errorf("storage: failed to delete config: %w", err)
	}
	log.L().Info("removed storage client", zap.Uint("configID", uint(configID)))
	return nil
}

// evict drops a config from the Redis and local caches, leaving the durable
// record alone. Called after any write that changes what the cached client
// should be.
func (m *ClientManager) evict(configID ConfigID) {
	if m.redisClient != nil {
		ctx := context.Background()
		if err := m.redisClient.Del(ctx, redisConfigKey(configID)).Err(); err != nil {
			// Best-effort: the cache entry expires anyway and is re-derived
			// from Postgres.
			log.L().Warn("failed to evict storage config from Redis cache",
				zap.Uint("configID", uint(configID)), zap.Error(err))
		}
	}
	m.localCache.Delete(localCacheKey(configID))
}

// HasClient reports whether configID has a config stored (Redis cache or the
// durable store). Checking the durable store means a Redis flush does not make
// configured storage appear unconfigured.
func (m *ClientManager) HasClient(configID ConfigID) bool {
	if m.redisClient != nil {
		ctx := context.Background()
		if exists, err := m.redisClient.Exists(ctx, redisConfigKey(configID)).Result(); err == nil && exists > 0 {
			return true
		}
	}
	return m.existsInDB(configID)
}

// Count returns the number of entries in the local cache (not Redis total).
func (m *ClientManager) Count() int {
	return m.localCache.ItemCount()
}

// ClearLocalCache evicts all entries from the local in-process cache.
// Redis is not affected.
func (m *ClientManager) ClearLocalCache() {
	m.localCache.Flush()
	log.L().Info("cleared local storage client cache")
}

// GetProviderConfig returns the stored ProviderConfig for configID.
// Returns nil, nil when no such config exists.
func (m *ClientManager) GetProviderConfig(configID ConfigID) (*ProviderConfig, error) {
	if m.redisClient == nil {
		// Fall back to the durable store so a Redis-less setup still resolves.
		return m.loadFromDB(configID)
	}
	return m.getConfigFromRedis(configID)
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func redisConfigKey(configID ConfigID) string {
	return fmt.Sprintf("%s%d", redisKeyPrefix, configID)
}

func localCacheKey(configID ConfigID) string {
	return strconv.FormatUint(uint64(configID), 10)
}

// encodeConfigForCache serialises an already-marshalled envelope for the Redis
// cache, encrypting it at rest when an encryption key is configured. This keeps
// the Redis copy consistent with the durable Postgres copy so secrets are not
// exposed via RDB/AOF snapshots, MONITOR, or a memory dump of Redis.
func (m *ClientManager) encodeConfigForCache(configJSON []byte) (string, error) {
	if m.box == nil {
		return string(configJSON), nil
	}
	return m.box.Encrypt(configJSON)
}

// decodeConfigFromCache reverses encodeConfigForCache. When a key is configured
// it expects ciphertext but tolerates legacy plaintext values written before
// encryption was enabled, so a rolling deploy need not flush Redis.
func (m *ClientManager) decodeConfigFromCache(raw string) (*ProviderConfig, error) {
	payload := []byte(raw)
	if m.box != nil {
		if pt, err := m.box.Decrypt(raw); err == nil {
			payload = pt
		}
		// Decrypt failure → fall back to treating raw as legacy plaintext JSON.
	}
	var cfg ProviderConfig
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return nil, fmt.Errorf("storage: failed to deserialize config: %w", err)
	}
	return &cfg, nil
}

func (m *ClientManager) getConfigFromRedis(configID ConfigID) (*ProviderConfig, error) {
	ctx := context.Background()
	raw, err := m.redisClient.Get(ctx, redisConfigKey(configID)).Result()
	if err == redis.Nil {
		// Cache miss — fall back to the durable store and repopulate the cache.
		// This is what makes the config survive a Redis flush/eviction.
		cfg, dbErr := m.loadFromDB(configID)
		if dbErr != nil {
			return nil, dbErr
		}
		if cfg == nil {
			return nil, nil
		}
		if rejson, mErr := json.Marshal(cfg); mErr == nil {
			if payload, encErr := m.encodeConfigForCache(rejson); encErr != nil {
				log.L().Warn("failed to encrypt storage config for Redis cache",
					zap.Uint("configID", uint(configID)), zap.Error(encErr))
			} else if sErr := m.redisClient.Set(ctx, redisConfigKey(configID), payload, 0).Err(); sErr != nil {
				log.L().Warn("failed to repopulate storage config cache from durable store",
					zap.Uint("configID", uint(configID)), zap.Error(sErr))
			}
		}
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: Redis get failed: %w", err)
	}

	return m.decodeConfigFromCache(raw)
}

func (m *ClientManager) createClientFromConfig(cfg *ProviderConfig) (StorageClient, error) {
	p, err := m.provider(cfg.Type)
	if err != nil {
		return nil, err
	}
	return p.CreateClient(cfg.RawConfig)
}
