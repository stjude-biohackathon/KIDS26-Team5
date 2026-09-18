package agent

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"antelope/internal/modules/agent/daytona"
	"antelope/internal/modules/storage"

	"trpc.group/trpc-go/trpc-agent-go/codeexecutor"
)

// sdkUploadMaxBytes is the size ceiling for staging an attachment by buffering
// it in the API process and uploading via the Daytona SDK (s3buf://). Larger
// files are streamed directly into the sandbox from a presigned URL (s3url://)
// so the API process never holds multi-GB genomics files in memory.
const sdkUploadMaxBytes = 32 << 20 // 32 MiB

// stagedInputsDir is the workspace-relative directory uploaded attachments land
// in. It mirrors the path the system prompt advertises to the model.
const stagedInputsDir = "work/inputs"

// storageObjectFetcher adapts a per-user storage.StorageClient to the
// daytona.ObjectFetcher interface. A nil client yields clear errors at stage
// time rather than a panic. primed maps "bucket/key" to bytes the chat
// service already downloaded for inlining, so staging a small attachment
// does not fetch the same object from storage a second time.
type storageObjectFetcher struct {
	c      storage.StorageClient
	primed map[string][]byte
}

func (f storageObjectFetcher) GetObject(ctx context.Context, bucket, key string) ([]byte, error) {
	if data, ok := f.primed[bucket+"/"+key]; ok {
		return data, nil
	}
	if f.c == nil {
		return nil, errors.New("storage client not configured for this user")
	}
	return f.c.GetObject(ctx, storage.GetObjectRequest{Bucket: bucket, Key: key})
}

func (f storageObjectFetcher) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	if f.c == nil {
		return "", errors.New("storage client not configured for this user")
	}
	return f.c.PresignedGetObject(ctx, bucket, key, ttl)
}

// newObjectFetcher builds the daytona fetcher for a user, primed with any
// attachment bytes already fetched this turn. Returns nil when the storage
// manager is absent so callers can skip wiring it.
func newObjectFetcher(sm *storage.ClientManager, userID uint, atts []AttachmentRef) daytona.ObjectFetcher {
	if sm == nil {
		return nil
	}
	primed := make(map[string][]byte, len(atts))
	for _, a := range atts {
		if len(a.Data) > 0 {
			primed[a.Bucket+"/"+a.Key] = a.Data
		}
	}
	return storageObjectFetcher{c: sm.PersonalClientForUser(userID), primed: primed}
}

// attachmentInputSpecs maps chat attachments to workspace InputSpecs. Each
// file is staged to its pinned StagedPath (set by the chat service; the
// StagedInputPaths entry is the fallback); the scheme (and thus transfer
// path) is chosen by size — Size is server-verified by the chat service
// before it reaches this point, so the buffered path can trust it. Pin lets
// the runtime skip re-staging a file already recorded at its destination
// this turn.
func attachmentInputSpecs(atts []AttachmentRef) []codeexecutor.InputSpec {
	paths := StagedInputPaths(atts)
	specs := make([]codeexecutor.InputSpec, 0, len(atts))
	for i, a := range atts {
		if a.Bucket == "" || a.Key == "" || a.Unavailable {
			continue
		}
		to := a.StagedPath
		if to == "" {
			to = paths[i]
		}
		// Default to the presigned/curl path: it is the safe choice for large
		// or unknown-size files. Only known-small files use the buffered path.
		scheme := daytona.SchemeS3URL
		if a.Size > 0 && a.Size <= sdkUploadMaxBytes {
			scheme = daytona.SchemeS3Buf
		}
		specs = append(specs, codeexecutor.InputSpec{
			From: scheme + a.Bucket + "/" + a.Key,
			To:   to,
			Pin:  true,
		})
	}
	return specs
}

// restoreArtifactRoots are the workspace-relative directories whose saved
// artifacts are restored into the fresh per-turn sandbox. They mirror the
// framework's artifact publish roots (workspace_save_artifact only accepts
// paths under work/, out/, runs/); names outside them — e.g. the
// inline/<ts>_<name> keys minted by run_python_inline — never lived on the
// workspace filesystem, so there is no path to restore them to.
var restoreArtifactRoots = []string{
	codeexecutor.DirWork,
	codeexecutor.DirOut,
	codeexecutor.DirRuns,
}

// sessionArtifactSpecs maps the session's saved artifacts to InputSpecs that
// restore each one at its original workspace-relative path. The sandbox is
// recreated every turn, so this is what lets turn N+1 read the out/ results
// turn N saved. Artifacts stage straight from their object-store location
// with the same size-based scheme choice as attachments; a new artifact
// version changes the object key, which naturally defeats a stale Pin.
func sessionArtifactSpecs(arts []SessionArtifactObject) []codeexecutor.InputSpec {
	specs := make([]codeexecutor.InputSpec, 0, len(arts))
	for _, a := range arts {
		rel, ok := restoreArtifactPath(a.Name)
		if !ok || a.Bucket == "" || a.Key == "" {
			continue
		}
		scheme := daytona.SchemeS3URL
		if a.Size > 0 && a.Size <= sdkUploadMaxBytes {
			scheme = daytona.SchemeS3Buf
		}
		specs = append(specs, codeexecutor.InputSpec{
			From: scheme + a.Bucket + "/" + a.Key,
			To:   rel,
			Pin:  true,
		})
	}
	return specs
}

// restoreArtifactPath reports whether an artifact name is a safe
// workspace-relative path under one of the restore roots, returning the
// destination to stage it at. Names with traversal, absolute, or non-clean
// components are rejected outright rather than normalised — they cannot have
// come from the framework's save path, which only accepts clean paths under
// the publish roots.
func restoreArtifactPath(name string) (string, bool) {
	rel := strings.TrimSpace(name)
	if rel == "" || rel != path.Clean(rel) || strings.HasPrefix(rel, "/") {
		return "", false
	}
	for _, root := range restoreArtifactRoots {
		if strings.HasPrefix(rel, root+"/") {
			return rel, true
		}
	}
	return "", false
}

// StagedInputPaths returns the workspace-relative staged path for each
// attachment, index-aligned with atts. Same-basename attachments are
// disambiguated deterministically (data.csv, data_2.csv, …) by input order.
// The stager (attachmentInputSpecs) and the user-facing message manifest
// both call this on the same attachment slice so the advertised paths never
// drift from the real ones.
func StagedInputPaths(atts []AttachmentRef) []string {
	out := make([]string, len(atts))
	used := make(map[string]bool, len(atts))
	for i, a := range atts {
		name := attachmentBaseName(a.Name)
		if used[name] {
			ext := path.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			for n := 2; ; n++ {
				cand := fmt.Sprintf("%s_%d%s", stem, n, ext)
				if !used[cand] {
					name = cand
					break
				}
			}
		}
		used[name] = true
		out[i] = path.Join(stagedInputsDir, name)
	}
	return out
}

// attachmentBaseName reduces a user-supplied filename to a safe basename for
// the staged path: no directory components, no traversal. Backslashes are
// normalised first so Windows-browser filenames cannot smuggle separators.
func attachmentBaseName(name string) string {
	b := path.Base(strings.TrimSpace(strings.ReplaceAll(name, "\\", "/")))
	if b == "" || b == "." || b == ".." || b == "/" {
		return "attachment"
	}
	return b
}
