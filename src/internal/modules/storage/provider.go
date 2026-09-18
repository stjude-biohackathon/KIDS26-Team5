package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ConfigID identifies a storage configuration record.
//
// Deliberately a distinct type rather than a bare uint. This module was
// previously keyed by user id, and every call site passed one; both are uint,
// so re-keying to configs would have compiled cleanly while silently reading
// the wrong row. The named type turns that into a compile error.
type ConfigID uint

// ProviderType identifies which object storage backend to use.
type ProviderType string

const (
	ProviderMinio ProviderType = "minio"
	ProviderS3    ProviderType = "s3" // reserved for future use
)

// BucketInfo is a backend-agnostic bucket descriptor.
type BucketInfo struct {
	Name      string
	CreatedAt time.Time
}

// PutObjectRequest carries the parameters for an upload operation.
type PutObjectRequest struct {
	Bucket      string
	Key         string
	Data        []byte
	ContentType string
}

// PutObjectStreamRequest carries the parameters for a streaming upload. The
// body is read from Reader; Size is the exact content length (-1 if unknown).
type PutObjectStreamRequest struct {
	Bucket      string
	Key         string
	Reader      io.Reader
	Size        int64
	ContentType string
}

// GetObjectRequest carries the parameters for a download operation.
type GetObjectRequest struct {
	Bucket string
	Key    string
}

// ObjectStat describes an object without downloading it. Used to validate
// client-reported metadata (notably size) before committing to an
// in-memory read.
type ObjectStat struct {
	Size         int64
	ContentType  string
	LastModified time.Time
}

// ObjectEntry is a backend-agnostic object descriptor used for
// streaming list/delete operations.
type ObjectEntry struct {
	Key          string
	Size         int64
	LastModified time.Time
	ContentType  string
	// ETag is the object's entity tag as reported by the backend. For
	// single-part, unencrypted uploads (both PutObject and presigned PUT)
	// this is the hex MD5 of the content, which callers use for cheap
	// change detection; multipart/encrypted objects carry other formats.
	ETag  string
	IsDir bool
	Err   error
}

// RemoveObjectError pairs an object key with the error that occurred
// during a bulk-delete operation.
type RemoveObjectError struct {
	Key string
	Err error
}

// StorageClient is the minimal interface every object storage backend must satisfy.
// The interface covers both core CRUD operations and the S3-compatible operations
// required by the file-browser (presigned URLs, bulk delete, bucket management).
type StorageClient interface {
	// ── Core CRUD ────────────────────────────────────────────────────────────
	ListBuckets(ctx context.Context) ([]BucketInfo, error)
	PutObject(ctx context.Context, req PutObjectRequest) error
	// PutObjectStream uploads from a reader without buffering the whole body
	// in memory. size is the exact content length; pass -1 only when it is
	// genuinely unknown (forces multipart). Used by the output harvest to
	// stream multi-GB sandbox files straight to storage.
	PutObjectStream(ctx context.Context, req PutObjectStreamRequest) error
	GetObject(ctx context.Context, req GetObjectRequest) ([]byte, error)
	// StatObject returns object metadata without downloading the body.
	// Callers MUST stat before GetObject when the expected size comes from
	// an untrusted source — GetObject buffers the whole object in memory.
	StatObject(ctx context.Context, bucket, key string) (ObjectStat, error)
	DeleteObject(ctx context.Context, bucket, key string) error

	// ── Bucket management ────────────────────────────────────────────────────
	BucketExists(ctx context.Context, bucket string) (bool, error)
	CreateBucket(ctx context.Context, bucket string) error
	RemoveBucket(ctx context.Context, bucket string) error

	// ── Object listing & bulk delete ─────────────────────────────────────────
	// ListObjects streams object entries for bucket/prefix. The returned channel
	// is closed when listing is complete or ctx is cancelled.
	ListObjects(ctx context.Context, bucket, prefix string, recursive bool) (<-chan ObjectEntry, error)
	// RemoveObjects bulk-deletes the objects emitted by the input channel and
	// streams any per-object errors on the returned channel.
	RemoveObjects(ctx context.Context, bucket string, objects <-chan ObjectEntry) <-chan RemoveObjectError

	// ── Presigned URLs ───────────────────────────────────────────────────────
	PresignedPutObject(ctx context.Context, bucket, key string, expiry time.Duration) (string, error)
	PresignedGetObject(ctx context.Context, bucket, key string, expiry time.Duration) (string, error)

	// ── Connectivity check ───────────────────────────────────────────────────
	// Ping verifies the connection is still alive (used by SetClient for validation).
	Ping(ctx context.Context) error
}

// ProviderConfig is the envelope stored in Redis.
// RawConfig holds the backend-specific JSON so the manager never needs to
// understand the inner structure.
type ProviderConfig struct {
	Type      ProviderType    `json:"type"`
	RawConfig json.RawMessage `json:"config"`
	Hash      string          `json:"hash"`
}

// StorageProvider is a factory that knows how to create a StorageClient
// from raw JSON config bytes.  Each backend (MinIO, S3, …) registers one.
type StorageProvider interface {
	// Type returns the ProviderType this factory handles.
	Type() ProviderType
	// CreateClient deserialises rawConfig and returns a ready-to-use client.
	CreateClient(rawConfig json.RawMessage) (StorageClient, error)
	// ComputeHash returns a stable hash of rawConfig for change detection.
	ComputeHash(rawConfig json.RawMessage) string
}

// defaultComputeHash is a shared helper for providers that don't need
// custom hash logic — just SHA-256 over the raw bytes.
func defaultComputeHash(rawConfig json.RawMessage) string {
	sum := sha256.Sum256(rawConfig)
	return hex.EncodeToString(sum[:])
}

// EndpointOf extracts the non-secret "host:port" identity of a backend so it
// can be stored in the clear and matched in SQL.
//
// The config hash covers credentials, so it only ever matches people who share
// one credential. Two researchers issued separate keys to the same server hash
// differently while pointing at identical data — which matters, because the
// classification of that data has to apply to both. Endpoint is what catches
// that case.
//
// Returns "" for providers whose shape is unknown; callers treat that as
// "cannot match by endpoint" rather than as a wildcard.
func EndpointOf(providerType ProviderType, rawConfig json.RawMessage) string {
	switch providerType {
	case ProviderMinio, ProviderS3:
		var cfg MinioConfig
		if err := json.Unmarshal(rawConfig, &cfg); err != nil {
			return ""
		}
		if cfg.Host == "" {
			return ""
		}
		return fmt.Sprintf("%s:%d", strings.ToLower(cfg.Host), cfg.Port)
	default:
		return ""
	}
}
