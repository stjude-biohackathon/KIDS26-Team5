package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"antelope/internal/modules/storage"
	"antelope/models"

	"gorm.io/gorm"
	"trpc.group/trpc-go/trpc-agent-go/artifact"
)

// objectKeyRoot is the prefix used inside the user's workspace bucket for
// agent-managed artifacts. All keys follow:
//
//	agent/{sessionID}/{filename}/{version}
//
// where filename is the framework-supplied logical name and version is a
// zero-based integer assigned at save time.
const objectKeyRoot = "agent"

// bucketCacheTTL controls how long a per-user workspace bucket lookup is
// cached in memory. AgentWorkspaceConfig is updated rarely (settings page);
// caching here saves a DB round-trip on every artifact call.
const bucketCacheTTL = 30 * time.Second

// PerUserS3Artifact implements artifact.Service by routing each call to the
// requesting user's MinIO/S3 client (resolved via storage.ClientManager)
// and the workspace bucket they configured in settings.
//
// SessionInfo.UserID arrives as a string from the framework; Antelope stores
// user IDs as uint. The conversion is done at the boundary; an unparseable
// string yields a clear error rather than a silent panic.
type PerUserS3Artifact struct {
	storage *storage.ClientManager
	db      *gorm.DB

	cacheMu sync.Mutex
	cache   map[uint]bucketCacheEntry
}

type bucketCacheEntry struct {
	bucket    string
	expiresAt time.Time
}

// Compile-time interface check.
var _ artifact.Service = (*PerUserS3Artifact)(nil)

// NewPerUserS3Artifact returns a ready service. Both storage and db must be
// non-nil; nil triggers a panic at construction (rather than per-call) since
// missing infra here is always a wiring bug.
func NewPerUserS3Artifact(storage *storage.ClientManager, db *gorm.DB) *PerUserS3Artifact {
	if storage == nil || db == nil {
		panic("agent.NewPerUserS3Artifact: storage and db are required")
	}
	return &PerUserS3Artifact{
		storage: storage,
		db:      db,
		cache:   make(map[uint]bucketCacheEntry),
	}
}

// SaveArtifact picks the next sequential version, uploads the bytes, and
// returns the assigned version. Not safe against concurrent saves to the
// same filename — matches the framework's own S3 backend behavior.
func (s *PerUserS3Artifact) SaveArtifact(
	ctx context.Context,
	info artifact.SessionInfo,
	filename string,
	art *artifact.Artifact,
) (int, error) {
	if art == nil {
		return 0, errors.New("artifact is nil")
	}
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return 0, err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return 0, err
	}

	prefix := objectPrefix(info.SessionID, filename)
	existing, err := s.listVersionsAtPrefix(ctx, client, bucket, prefix)
	if err != nil {
		return 0, fmt.Errorf("list versions: %w", err)
	}
	next := 0
	if len(existing) > 0 {
		next = existing[len(existing)-1] + 1
	}

	key := path.Join(prefix, strconv.Itoa(next))
	mime := art.MimeType
	if mime == "" {
		mime = "application/octet-stream"
	}
	if err := client.PutObject(ctx, storage.PutObjectRequest{
		Bucket:      bucket,
		Key:         key,
		Data:        art.Data,
		ContentType: mime,
	}); err != nil {
		return 0, fmt.Errorf("put object %s: %w", key, err)
	}
	return next, nil
}

// LoadArtifact returns the requested version, or the most recent version
// when version is nil. Returns nil (no error) when the artifact does not
// exist — matches the framework's inmemory backend semantics.
func (s *PerUserS3Artifact) LoadArtifact(
	ctx context.Context,
	info artifact.SessionInfo,
	filename string,
	version *int,
) (*artifact.Artifact, error) {
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return nil, err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return nil, err
	}

	prefix := objectPrefix(info.SessionID, filename)
	resolved, err := s.resolveVersion(ctx, client, bucket, prefix, version)
	if err != nil {
		return nil, err
	}
	if resolved < 0 {
		return nil, nil
	}

	key := path.Join(prefix, strconv.Itoa(resolved))
	data, err := client.GetObject(ctx, storage.GetObjectRequest{Bucket: bucket, Key: key})
	if err != nil {
		return nil, fmt.Errorf("get object %s: %w", key, err)
	}
	mime, _ := s.contentType(ctx, client, bucket, key)
	return &artifact.Artifact{
		Data:     data,
		MimeType: mime,
		Name:     path.Base(filename),
	}, nil
}

// ListArtifactKeys returns the unique filenames for a session. The framework
// expects logical names, not S3 keys, so we strip the object-key suffix.
func (s *PerUserS3Artifact) ListArtifactKeys(
	ctx context.Context,
	info artifact.SessionInfo,
) ([]string, error) {
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return nil, err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return nil, err
	}

	sessionRoot := path.Join(objectKeyRoot, info.SessionID) + "/"
	entries, err := client.ListObjects(ctx, bucket, sessionRoot, true)
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}

	seen := make(map[string]struct{})
	for e := range entries {
		if e.Err != nil {
			return nil, e.Err
		}
		if e.IsDir {
			continue
		}
		name := keyToFilename(sessionRoot, e.Key)
		if name == "" {
			continue
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// SessionArtifactObject locates the latest stored version of one logical
// session artifact in the user's workspace bucket. Exposed for workspace
// re-staging: the agent factory turns each entry into an s3 stage spec so a
// fresh per-turn sandbox starts with the artifacts earlier turns produced
// (see sessionArtifactSpecs in staging.go).
type SessionArtifactObject struct {
	Name    string // logical artifact name, e.g. "out/result.png"
	Bucket  string // user's workspace bucket
	Key     string // object key of the latest version
	Version int
	Size    int64
	// ETag is the latest version's entity tag. Artifact writes force
	// single-part PUTs (see storage.PutObject), so for an unencrypted bucket
	// this is the hex content MD5. The output harvest compares it against the
	// in-sandbox MD5 to skip files whose content is already stored.
	ETag string
}

// ListSessionArtifacts returns the latest version of every artifact saved in
// the session, sorted by name. A single recursive LIST over the session
// prefix yields name, key, and size together, so callers staging the files
// don't need a per-artifact ListVersions/Stat round-trip.
func (s *PerUserS3Artifact) ListSessionArtifacts(
	ctx context.Context,
	info artifact.SessionInfo,
) ([]SessionArtifactObject, error) {
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return nil, err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return nil, err
	}

	sessionRoot := path.Join(objectKeyRoot, info.SessionID) + "/"
	entries, err := client.ListObjects(ctx, bucket, sessionRoot, true)
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}

	latest := make(map[string]SessionArtifactObject)
	for e := range entries {
		if e.Err != nil {
			return nil, e.Err
		}
		if e.IsDir {
			continue
		}
		name := keyToFilename(sessionRoot, e.Key)
		if name == "" {
			continue
		}
		ver, err := strconv.Atoi(path.Base(e.Key))
		if err != nil {
			continue // skip unrelated objects under the prefix
		}
		if cur, ok := latest[name]; !ok || ver > cur.Version {
			latest[name] = SessionArtifactObject{
				Name:    name,
				Bucket:  bucket,
				Key:     e.Key,
				Version: ver,
				Size:    e.Size,
				ETag:    e.ETag,
			}
		}
	}
	out := make([]SessionArtifactObject, 0, len(latest))
	for _, a := range latest {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteArtifact removes every version of the given filename.
func (s *PerUserS3Artifact) DeleteArtifact(
	ctx context.Context,
	info artifact.SessionInfo,
	filename string,
) error {
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return err
	}

	prefix := objectPrefix(info.SessionID, filename) + "/"
	entries, err := client.ListObjects(ctx, bucket, prefix, true)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	// Pump into RemoveObjects (it accepts a channel and bulk-deletes).
	errCh := client.RemoveObjects(ctx, bucket, entries)
	for re := range errCh {
		if re.Err != nil {
			return fmt.Errorf("remove %s: %w", re.Key, re.Err)
		}
	}
	return nil
}

// DeleteSessionArtifacts removes every artifact the session produced — all
// logical files, all versions — by wiping the agent/{sessionID}/ prefix.
// Idempotent: a session with nothing stored (or whose objects were already
// deleted) lists empty and returns nil rather than erroring, so it is safe to
// call during conversation teardown even after a manual cleanup.
func (s *PerUserS3Artifact) DeleteSessionArtifacts(
	ctx context.Context,
	info artifact.SessionInfo,
) error {
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return err
	}

	sessionRoot := path.Join(objectKeyRoot, info.SessionID) + "/"
	entries, err := client.ListObjects(ctx, bucket, sessionRoot, true)
	if err != nil {
		return fmt.Errorf("list session artifacts: %w", err)
	}
	errCh := client.RemoveObjects(ctx, bucket, entries)
	for re := range errCh {
		if re.Err != nil {
			return fmt.Errorf("remove %s: %w", re.Key, re.Err)
		}
	}
	return nil
}

// ListVersions returns every version of the given filename, sorted ascending.
func (s *PerUserS3Artifact) ListVersions(
	ctx context.Context,
	info artifact.SessionInfo,
	filename string,
) ([]int, error) {
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return nil, err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return nil, err
	}
	return s.listVersionsAtPrefix(ctx, client, bucket, objectPrefix(info.SessionID, filename))
}

// PresignedURL returns a presigned GET URL the frontend can use to download
// the artifact. NOT part of artifact.Service — exposed for the HTTP layer.
func (s *PerUserS3Artifact) PresignedURL(
	ctx context.Context,
	info artifact.SessionInfo,
	filename string,
	version int,
	ttl time.Duration,
) (string, error) {
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return "", err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return "", err
	}
	key := path.Join(objectPrefix(info.SessionID, filename), strconv.Itoa(version))
	return client.PresignedGetObject(ctx, bucket, key, ttl)
}

// SaveArtifactStream stores an artifact by streaming from r, picking the next
// sequential version exactly like SaveArtifact. NOT part of artifact.Service —
// exposed for the output harvest's buffered fallback so a multi-GB sandbox
// file streams through the API process (sandbox → API → storage) with flat
// heap usage instead of being read fully into memory. size is the exact
// content length (-1 if unknown). Returns the assigned version.
func (s *PerUserS3Artifact) SaveArtifactStream(
	ctx context.Context,
	info artifact.SessionInfo,
	filename string,
	r io.Reader,
	size int64,
	contentType string,
) (int, error) {
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return 0, err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return 0, err
	}

	prefix := objectPrefix(info.SessionID, filename)
	existing, err := s.listVersionsAtPrefix(ctx, client, bucket, prefix)
	if err != nil {
		return 0, fmt.Errorf("list versions: %w", err)
	}
	next := 0
	if len(existing) > 0 {
		next = existing[len(existing)-1] + 1
	}

	mime := contentType
	if mime == "" {
		mime = "application/octet-stream"
	}
	key := path.Join(prefix, strconv.Itoa(next))
	if err := client.PutObjectStream(ctx, storage.PutObjectStreamRequest{
		Bucket:      bucket,
		Key:         key,
		Reader:      r,
		Size:        size,
		ContentType: mime,
	}); err != nil {
		return 0, fmt.Errorf("put object stream %s: %w", key, err)
	}
	return next, nil
}

// PresignedPutURL returns a presigned PUT URL for one specific version slot
// of an artifact. NOT part of artifact.Service — exposed for the end-of-turn
// output harvest, which uploads straight from the sandbox so large files
// never pass through the API process. The caller owns version allocation
// (next = latest + 1 from ListSessionArtifacts).
func (s *PerUserS3Artifact) PresignedPutURL(
	ctx context.Context,
	info artifact.SessionInfo,
	filename string,
	version int,
	ttl time.Duration,
) (string, error) {
	uid, err := parseUserID(info.UserID)
	if err != nil {
		return "", err
	}
	client, bucket, err := s.clientFor(ctx, uid)
	if err != nil {
		return "", err
	}
	key := path.Join(objectPrefix(info.SessionID, filename), strconv.Itoa(version))
	return client.PresignedPutObject(ctx, bucket, key, ttl)
}

// ── helpers ─────────────────────────────────────────────────────────────────

// clientFor resolves both the per-user storage client and the workspace
// bucket from settings.
func (s *PerUserS3Artifact) clientFor(ctx context.Context, userID uint) (storage.StorageClient, string, error) {
	client := s.storage.PersonalClientForUser(userID)
	if client == nil {
		return nil, "", fmt.Errorf("storage is not configured for user %d", userID)
	}
	bucket, err := s.resolveBucket(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	return client, bucket, nil
}

// resolveBucket reads the user's chosen workspace bucket, with a short
// in-memory cache to avoid DB hits on every artifact call.
func (s *PerUserS3Artifact) resolveBucket(ctx context.Context, userID uint) (string, error) {
	now := time.Now()
	s.cacheMu.Lock()
	if entry, ok := s.cache[userID]; ok && now.Before(entry.expiresAt) {
		s.cacheMu.Unlock()
		return entry.bucket, nil
	}
	s.cacheMu.Unlock()

	var ws models.AgentWorkspaceConfig
	err := s.db.WithContext(ctx).Where("user_id = ?", userID).First(&ws).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", fmt.Errorf("agent workspace bucket is not configured for user %d", userID)
		}
		return "", fmt.Errorf("load workspace config: %w", err)
	}
	bucket := strings.TrimSpace(ws.Bucket)
	if bucket == "" {
		return "", fmt.Errorf("agent workspace bucket is empty for user %d", userID)
	}

	s.cacheMu.Lock()
	s.cache[userID] = bucketCacheEntry{bucket: bucket, expiresAt: now.Add(bucketCacheTTL)}
	s.cacheMu.Unlock()
	return bucket, nil
}

// InvalidateBucket evicts the cached bucket for a user. Call after a user
// updates their AgentWorkspaceConfig.
func (s *PerUserS3Artifact) InvalidateBucket(userID uint) {
	s.cacheMu.Lock()
	delete(s.cache, userID)
	s.cacheMu.Unlock()
}

// listVersionsAtPrefix lists every object directly beneath the artifact's
// prefix and returns the parsed version integers in ascending order.
func (s *PerUserS3Artifact) listVersionsAtPrefix(
	ctx context.Context,
	client storage.StorageClient,
	bucket, prefix string,
) ([]int, error) {
	entries, err := client.ListObjects(ctx, bucket, prefix+"/", true)
	if err != nil {
		return nil, err
	}
	var versions []int
	for e := range entries {
		if e.Err != nil {
			return nil, e.Err
		}
		if e.IsDir {
			continue
		}
		base := path.Base(e.Key)
		v, err := strconv.Atoi(base)
		if err != nil {
			continue // skip unrelated objects under the prefix
		}
		versions = append(versions, v)
	}
	sort.Ints(versions)
	return versions, nil
}

// resolveVersion returns the requested version, or the highest available
// version when version is nil. Returns -1 (no error) when the prefix has no
// objects, so the caller can distinguish "not found" from real failures.
func (s *PerUserS3Artifact) resolveVersion(
	ctx context.Context,
	client storage.StorageClient,
	bucket, prefix string,
	version *int,
) (int, error) {
	if version != nil {
		return *version, nil
	}
	versions, err := s.listVersionsAtPrefix(ctx, client, bucket, prefix)
	if err != nil {
		return -1, err
	}
	if len(versions) == 0 {
		return -1, nil
	}
	return versions[len(versions)-1], nil
}

// contentType fetches Content-Type via a one-element ListObjects since the
// minimal StorageClient interface doesn't expose StatObject. The lookup is
// best-effort; an error or missing type yields an empty string.
func (s *PerUserS3Artifact) contentType(
	ctx context.Context,
	client storage.StorageClient,
	bucket, key string,
) (string, error) {
	entries, err := client.ListObjects(ctx, bucket, key, false)
	if err != nil {
		return "", err
	}
	for e := range entries {
		if e.Err != nil {
			return "", e.Err
		}
		if e.Key == key {
			return e.ContentType, nil
		}
	}
	return "", nil
}

// objectPrefix is the directory holding every version of one artifact.
func objectPrefix(sessionID, filename string) string {
	return path.Join(objectKeyRoot, sessionID, filename)
}

// keyToFilename extracts the logical filename from a full object key.
// Given root="agent/{sid}/" and key="agent/{sid}/plot.png/0", returns
// "plot.png". Returns "" if the key does not have the expected shape.
func keyToFilename(sessionRoot, key string) string {
	rel := strings.TrimPrefix(key, sessionRoot)
	if rel == key {
		return ""
	}
	// rel looks like "{filename}/{version}"; trim the trailing version.
	idx := strings.LastIndex(rel, "/")
	if idx < 0 {
		return ""
	}
	return rel[:idx]
}

func parseUserID(s string) (uint, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("session user_id is empty")
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid user id %q: %w", s, err)
	}
	return uint(n), nil
}
