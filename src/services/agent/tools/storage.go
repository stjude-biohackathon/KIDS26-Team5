package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"antelope/internal/modules/agent"
	"antelope/internal/modules/storage"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type browseStorageTool struct {
	storage *storage.ClientManager
}

// NewBrowseStorageTool returns a tool that lists buckets (when no bucket
// argument is given) or objects in a bucket. Results are scoped to the
// requesting user's MinIO/S3 instance.
func NewBrowseStorageTool(mgr *storage.ClientManager) tool.CallableTool {
	return &browseStorageTool{storage: mgr}
}

func (t *browseStorageTool) Declaration() *tool.Declaration {
	return &tool.Declaration{
		Name:        "browse_storage",
		Description: "Browse the user's MinIO/S3 storage. Leave 'bucket' empty to list buckets; provide 'bucket' (and optionally 'prefix') to list objects.",
		InputSchema: &tool.Schema{
			Type: "object",
			Properties: map[string]*tool.Schema{
				"bucket": {Type: "string", Description: "Bucket name. Leave empty to list buckets."},
				"prefix": {Type: "string", Description: "Object prefix to filter by (optional, only used when bucket is set)."},
			},
		},
	}
}

type browseStorageInput struct {
	Bucket string `json:"bucket,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

type bucketInfo struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type objectInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
}

func (t *browseStorageTool) Call(ctx context.Context, jsonArgs []byte) (any, error) {
	userID, ok := agent.UserIDFromContext(ctx)
	if !ok {
		return nil, errors.New("user id missing from context")
	}
	var in browseStorageInput
	if len(jsonArgs) > 0 {
		if err := json.Unmarshal(jsonArgs, &in); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
	}

	client := t.storage.PersonalClientForUser(userID)
	if client == nil {
		return map[string]any{"error": "no storage is configured for this user"}, nil
	}

	if in.Bucket == "" {
		buckets, err := client.ListBuckets(ctx)
		if err != nil {
			return nil, fmt.Errorf("list buckets: %w", err)
		}
		out := make([]bucketInfo, 0, len(buckets))
		for _, b := range buckets {
			out = append(out, bucketInfo{Name: b.Name, CreatedAt: b.CreatedAt})
		}
		return out, nil
	}

	entries, err := client.ListObjects(ctx, in.Bucket, in.Prefix, true)
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	var out []objectInfo
	for e := range entries {
		if e.Err != nil {
			return nil, e.Err
		}
		if e.IsDir {
			continue
		}
		out = append(out, objectInfo{Key: e.Key, Size: e.Size, LastModified: e.LastModified})
	}
	return out, nil
}

// ── get_download_url ──────────────────────────────────────────────────────────

// downloadURLTTL is how long a generated object download URL stays valid.
// Matches the OSS handler's 24h window so agent-issued links behave like
// links the file browser hands out.
const downloadURLTTL = 24 * time.Hour

type getDownloadURLTool struct {
	storage *storage.ClientManager
}

// NewGetDownloadURLTool returns a tool that mints a presigned GET URL for an
// object in the user's storage, so the agent can hand the user a direct
// download link. Read-only: it grants time-limited GET access, never writes.
func NewGetDownloadURLTool(mgr *storage.ClientManager) tool.CallableTool {
	return &getDownloadURLTool{storage: mgr}
}

func (t *getDownloadURLTool) Declaration() *tool.Declaration {
	return &tool.Declaration{
		Name:        "get_download_url",
		Description: "Generate a temporary download URL (valid 24h) for an object in the user's MinIO/S3 storage. Use browse_storage first to find the bucket and key.",
		InputSchema: &tool.Schema{
			Type:     "object",
			Required: []string{"bucket", "key"},
			Properties: map[string]*tool.Schema{
				"bucket": {Type: "string", Description: "Bucket containing the object."},
				"key":    {Type: "string", Description: "Object key (full path within the bucket)."},
			},
		},
	}
}

type getDownloadURLInput struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

func (t *getDownloadURLTool) Call(ctx context.Context, jsonArgs []byte) (any, error) {
	userID, ok := agent.UserIDFromContext(ctx)
	if !ok {
		return nil, errors.New("user id missing from context")
	}
	var in getDownloadURLInput
	if err := json.Unmarshal(jsonArgs, &in); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if in.Bucket == "" || in.Key == "" {
		return nil, errors.New("bucket and key are required")
	}

	client := t.storage.PersonalClientForUser(userID)
	if client == nil {
		return map[string]any{"error": "no storage is configured for this user"}, nil
	}
	url, err := client.PresignedGetObject(ctx, in.Bucket, in.Key, downloadURLTTL)
	if err != nil {
		return nil, fmt.Errorf("presign download url: %w", err)
	}
	return map[string]any{
		"download_url": url,
		"expires_at":   time.Now().Add(downloadURLTTL).UTC(),
	}, nil
}
