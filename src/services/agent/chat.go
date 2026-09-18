package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	agentmod "antelope/internal/modules/agent"
	"antelope/internal/modules/log"
	"antelope/internal/modules/sse"
	"antelope/internal/modules/storage"
	"antelope/pkg/apperr"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"trpc.group/trpc-go/trpc-agent-go/artifact"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

// CreateConversation creates a new session in the framework's postgres-
// backed session store. The conversation ID is a fresh UUID; titles are
// stored in session state under agentmod.SessionStateTitle.
func (s *service) CreateConversation(ctx context.Context, userID uint, title string) (gin.H, error) {
	if userID == 0 {
		return nil, errUserIDRequired
	}
	if title == "" {
		title = "New Conversation"
	}
	key := session.Key{AppName: s.cfg.AppName, UserID: userIDToString(userID), SessionID: uuid.NewString()}
	state := session.StateMap{agentmod.SessionStateTitle: []byte(title)}
	sess, err := s.session.CreateSession(ctx, key, state)
	if err != nil {
		log.L().Error("create session failed", zap.Error(err))
		return nil, apperr.ServerError("failed to create conversation")
	}
	return gin.H{
		"id":         sess.ID,
		"title":      title,
		"created_at": sess.CreatedAt,
		"updated_at": sess.UpdatedAt,
	}, nil
}

// ListConversations returns a page of the user's conversation summaries (id,
// title, timestamps) sorted by most-recent first. We pass WithListSessionOnlyMeta
// so the backend can skip loading events and tracks, and WithListSessionPage for
// offset/limit pagination (the session service orders by updated_at DESC).
//
// has_more is true when the page is full (len == limit), signalling the caller
// that another page may exist — this avoids a separate full-table count.
func (s *service) ListConversations(ctx context.Context, userID uint, limit, offset int) (gin.H, error) {
	if userID == 0 {
		return nil, errUserIDRequired
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	userKey := session.UserKey{AppName: s.cfg.AppName, UserID: userIDToString(userID)}
	sessions, err := s.session.ListSessions(ctx, userKey,
		session.WithListSessionOnlyMeta(),
		session.WithListSessionPage(offset, limit),
	)
	if err != nil {
		log.L().Error("list sessions failed", zap.Error(err))
		return nil, apperr.ServerError("failed to list conversations")
	}
	items := make([]gin.H, 0, len(sessions))
	for _, sess := range sessions {
		items = append(items, gin.H{
			"id":         sess.ID,
			"title":      readTitle(sess),
			"created_at": sess.CreatedAt,
			"updated_at": sess.UpdatedAt,
		})
	}
	return gin.H{"items": items, "has_more": len(sessions) == limit}, nil
}

// GetConversation loads a single conversation including all of its events,
// serialized as a message list the frontend can render.
func (s *service) GetConversation(ctx context.Context, userID uint, sessionID string) (gin.H, error) {
	if userID == 0 {
		return nil, errUserIDRequired
	}
	key := session.Key{AppName: s.cfg.AppName, UserID: userIDToString(userID), SessionID: sessionID}
	sess, err := s.session.GetSession(ctx, key)
	if err != nil {
		log.L().Error("get session failed", zap.Error(err))
		return nil, apperr.ServerError("failed to load conversation")
	}
	if sess == nil {
		return nil, apperr.NotFound("conversation not found")
	}
	return gin.H{
		"id":             sess.ID,
		"title":          readTitle(sess),
		"created_at":     sess.CreatedAt,
		"updated_at":     sess.UpdatedAt,
		"messages":       serializeEvents(sess),
		"context_tokens": latestContextTokens(sess),
	}, nil
}

// latestContextTokens returns the prompt-token count of the most recent event
// that carries provider usage — i.e. the size of the full context last sent to
// the model. Returns 0 when no usage was recorded. Drives the UI context-usage
// meter when a conversation is reopened.
func latestContextTokens(sess *session.Session) int {
	events := sess.GetEvents()
	for _, v := range slices.Backward(events) {
		e := &v
		if e.Response != nil && e.Response.Usage != nil && e.Response.Usage.PromptTokens > 0 {
			return e.Response.Usage.PromptTokens
		}
	}
	return 0
}

// DeleteConversation removes a session, its events, and every file it owns:
// the agent's session artifacts and the chat attachments the user uploaded.
// Sandbox teardown and file cleanup run first so a discarded conversation
// stops costing the user storage. Both are best-effort and idempotent: a
// storage hiccup, an already-deleted object, or an unreachable Daytona never
// blocks the session delete, which is the authoritative action.
func (s *service) DeleteConversation(ctx context.Context, userID uint, sessionID string) error {
	if userID == 0 {
		return errUserIDRequired
	}
	key := session.Key{AppName: s.cfg.AppName, UserID: userIDToString(userID), SessionID: sessionID}

	if err := s.agent.KillSandbox(ctx, userID, sessionID); err != nil {
		log.L().Warn("delete conversation: cleanup sandbox failed (continuing)",
			zap.Uint("user_id", userID),
			zap.String("session_id", sessionID),
			zap.Error(err))
	}

	s.deleteConversationFiles(ctx, userID, sessionID, key)

	if err := s.session.DeleteSession(ctx, key); err != nil {
		log.L().Error("delete session failed", zap.Error(err))
		return apperr.ServerError("failed to delete conversation")
	}
	return nil
}

// deleteConversationFiles best-effort removes the two classes of stored files a
// conversation owns: the agent's session artifacts (under agent/{sessionID}/)
// and the chat attachments the user uploaded (whose object keys are recorded in
// session state). Every step is idempotent and logged-but-never-fatal — objects
// the user already deleted are silently skipped, and a partial failure must not
// stop the conversation from being deleted. The session is read before it is
// removed so the attachment keys are still available.
func (s *service) deleteConversationFiles(ctx context.Context, userID uint, sessionID string, key session.Key) {
	// 1. Session artifacts (everything the agent saved this conversation).
	info := artifact.SessionInfo{
		AppName:   s.cfg.AppName,
		UserID:    userIDToString(userID),
		SessionID: sessionID,
	}
	if err := s.agent.Artifacts().DeleteSessionArtifacts(ctx, info); err != nil {
		log.L().Warn("delete conversation: remove session artifacts failed (continuing)",
			zap.Uint("user_id", userID),
			zap.String("session_id", sessionID),
			zap.Error(err))
	}

	// 2. Uploaded chat attachments. Their bucket/key live in session state, so
	//    load the session (still present at this point) to recover them.
	if s.storage == nil {
		return
	}
	sess, err := s.session.GetSession(ctx, key)
	if err != nil {
		log.L().Warn("delete conversation: load session for attachment cleanup failed (continuing)",
			zap.String("session_id", sessionID), zap.Error(err))
		return
	}
	atts := priorStagedAttachments(sess)
	if len(atts) == 0 {
		return
	}
	client := s.storage.PersonalClientForUser(userID)
	if client == nil {
		return
	}
	for _, a := range atts {
		if a.Bucket == "" || a.Key == "" {
			continue
		}
		// DeleteObject is idempotent on S3/MinIO — a missing key is a no-op.
		if err := client.DeleteObject(ctx, a.Bucket, a.Key); err != nil {
			log.L().Warn("delete conversation: remove attachment object failed (continuing)",
				zap.String("bucket", a.Bucket),
				zap.String("key", a.Key),
				zap.Error(err))
		}
	}
}

// SendMessageStream is the SSE entry-point for one chat turn. It wires an
// AgentSSESource into the existing sse.Manager so cancellation and writer
// lifecycle match the rest of the codebase. The handler returns when the
// runner emits its completion event or the client disconnects.
func (s *service) SendMessageStream(
	c *gin.Context,
	userID uint,
	sessionID string,
	content string,
	attachments []agentmod.AttachmentRef,
) {
	if userID == 0 || sessionID == "" {
		c.JSON(400, gin.H{"code": 400, "msg": "user id and session id are required"})
		return
	}
	writer, err := sse.NewWriter(c.Writer)
	if err != nil {
		c.JSON(500, gin.H{"code": 500, "msg": "sse setup failed"})
		return
	}

	// The session is loaded once per message: the first-message title
	// derivation and the staged-attachment state both read from it.
	key := session.Key{AppName: s.cfg.AppName, UserID: userIDToString(userID), SessionID: sessionID}
	sess, err := s.session.GetSession(c.Request.Context(), key)
	if err != nil {
		log.L().Warn("get session failed (continuing without prior state)", zap.Error(err))
	}

	// On the first user message of a conversation, derive a meaningful
	// title from the content so the conversation list shows something
	// better than "New Conversation". This stays out of the agent loop —
	// just a small state write before the runner kicks off.
	newTitle := s.maybeSetTitleFromFirstMessage(c.Request.Context(), key, sess, content)

	var client storage.StorageClient
	if s.storage != nil {
		client = s.storage.PersonalClientForUser(userID)
	}
	// Replace client-reported attachment sizes with stat'ed truth before
	// anything uses Size for memory or transfer decisions.
	normalizeAttachments(c.Request.Context(), client, attachments)

	// Pin each attachment to the staged path its manifest will advertise,
	// then bring back the attachments of earlier turns: the sandbox is
	// recreated every turn, so without re-staging them the paths promised
	// by old message manifests would dangle.
	for i, p := range agentmod.StagedInputPaths(attachments) {
		attachments[i].StagedPath = p
	}
	prior := priorStagedAttachments(sess)
	s.persistStagedAttachments(c.Request.Context(), key, mergeStagedAttachments(prior, attachments))

	msg := s.buildUserMessage(c.Request.Context(), client, content, attachments)
	src := newAgentSSESource(s.agent, userID, sessionID, msg, append(prior, attachments...))
	src.initialTitle = newTitle
	if err := s.sse.Serve(c.Request.Context(), writer, src); err != nil {
		log.L().Warn("agent stream ended with error", zap.Error(err))
	}
}

// normalizeAttachments overwrites each attachment's client-reported Size
// with the object's real size from storage. The wire Size is untrusted —
// a wrong value would route a multi-GB object through the in-memory
// transfer paths. Attachments whose objects cannot be stat'ed (deleted,
// never uploaded, wrong key) are marked Unavailable so they are excluded
// from inlining and staging and flagged in the message manifest.
func normalizeAttachments(ctx context.Context, client storage.StorageClient, atts []agentmod.AttachmentRef) {
	for i := range atts {
		a := &atts[i]
		if a.Bucket == "" || a.Key == "" {
			a.Unavailable = true
			continue
		}
		if client == nil {
			a.Unavailable = true
			continue
		}
		stat, err := client.StatObject(ctx, a.Bucket, a.Key)
		if err != nil {
			log.L().Warn("attachment stat failed; marking unavailable",
				zap.String("bucket", a.Bucket), zap.String("key", a.Key), zap.Error(err))
			a.Unavailable = true
			continue
		}
		a.Size = stat.Size
		if a.MimeType == "" {
			a.MimeType = stat.ContentType
		}
	}
}

// maybeSetTitleFromFirstMessage persists a derived title when the session
// is still using the default name and has no events. Returns the new
// title (or "" when nothing was updated) so the SSE source can echo it
// to the client without a separate API round-trip.
func (s *service) maybeSetTitleFromFirstMessage(ctx context.Context, key session.Key, sess *session.Session, content string) string {
	if sess == nil {
		return ""
	}
	current := readTitle(sess)
	if current != "" && current != "New Conversation" {
		return ""
	}
	if sess.GetEventCount() > 0 {
		return ""
	}
	newTitle := deriveTitleFromMessage(content)
	if newTitle == "" {
		return ""
	}
	state := session.StateMap{agentmod.SessionStateTitle: []byte(newTitle)}
	if err := s.session.UpdateSessionState(ctx, key, state); err != nil {
		log.L().Warn("update conversation title failed", zap.Error(err))
		return ""
	}
	return newTitle
}

// priorStagedAttachments decodes the attachments staged by earlier turns
// from session state. Best-effort: a missing or corrupt entry just means
// nothing is restored this turn. Entries that cannot be staged (no source
// object or no pinned path) are filtered out.
func priorStagedAttachments(sess *session.Session) []agentmod.AttachmentRef {
	if sess == nil {
		return nil
	}
	raw, ok := sess.GetState(agentmod.SessionStateStagedAttachments)
	if !ok || len(raw) == 0 {
		return nil
	}
	var atts []agentmod.AttachmentRef
	if err := json.Unmarshal(raw, &atts); err != nil {
		log.L().Warn("decode staged attachments state failed; ignoring", zap.Error(err))
		return nil
	}
	out := make([]agentmod.AttachmentRef, 0, len(atts))
	for _, a := range atts {
		if stageableAttachment(a) {
			out = append(out, a)
		}
	}
	return out
}

// mergeStagedAttachments combines the prior staged set with this turn's
// attachments, deduplicating by staged path. The current turn wins a path
// collision — its manifest is the one the model sees claiming that path.
func mergeStagedAttachments(prior, current []agentmod.AttachmentRef) []agentmod.AttachmentRef {
	owned := make(map[string]bool, len(current))
	for _, a := range current {
		if stageableAttachment(a) {
			owned[a.StagedPath] = true
		}
	}
	out := make([]agentmod.AttachmentRef, 0, len(prior)+len(current))
	for _, a := range prior {
		if !stageableAttachment(a) || owned[a.StagedPath] {
			continue
		}
		owned[a.StagedPath] = true
		out = append(out, a)
	}
	for _, a := range current {
		if stageableAttachment(a) {
			out = append(out, a)
		}
	}
	return out
}

// stageableAttachment reports whether an attachment can be re-staged in a
// later turn: it needs a source object and the workspace path it was
// advertised at. Unavailable attachments were never staged to begin with.
func stageableAttachment(a agentmod.AttachmentRef) bool {
	return !a.Unavailable && a.Bucket != "" && a.Key != "" && a.StagedPath != ""
}

// persistStagedAttachments writes the merged staged-attachment set to
// session state so the next turn can restore every staged file into its
// fresh sandbox. Best-effort: on failure the next turn just restores less.
func (s *service) persistStagedAttachments(ctx context.Context, key session.Key, atts []agentmod.AttachmentRef) {
	if len(atts) == 0 {
		return
	}
	data, err := json.Marshal(atts)
	if err != nil {
		log.L().Warn("encode staged attachments state failed", zap.Error(err))
		return
	}
	state := session.StateMap{agentmod.SessionStateStagedAttachments: data}
	if err := s.session.UpdateSessionState(ctx, key, state); err != nil {
		log.L().Warn("update staged attachments state failed", zap.Error(err))
	}
}

// deriveTitleFromMessage turns the first user message into a short title.
// It clips at a word boundary inside ~40 chars and appends an ellipsis
// when truncated. Returns "" only when the input is blank.
func deriveTitleFromMessage(content string) string {
	s := strings.TrimSpace(content)
	if s == "" {
		return ""
	}
	// Collapse internal whitespace so the title stays one line.
	s = strings.Join(strings.Fields(s), " ")
	const maxLen = 40
	if len([]rune(s)) <= maxLen {
		return s
	}
	runes := []rune(s)
	truncated := string(runes[:maxLen])
	if idx := strings.LastIndex(truncated, " "); idx > maxLen/2 {
		truncated = truncated[:idx]
	}
	return truncated + "…"
}

// ── helpers ─────────────────────────────────────────────────────────────────

func userIDToString(userID uint) string {
	return strconv.FormatUint(uint64(userID), 10)
}

func readTitle(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	v, ok := sess.GetState(agentmod.SessionStateTitle)
	if !ok || len(v) == 0 {
		return "New Conversation"
	}
	return string(v)
}

// Inline-attachment limits. Small text-like attachments are fetched from the
// user's workspace bucket and embedded directly in the user message so the
// model reads them without a separate tool call. Every attachment — inlined
// or not — is also staged into the sandbox workspace. The caps keep a single
// turn from blowing the context window.
const (
	maxInlineFileBytes  = 256 * 1024 // per-file ceiling
	maxInlineTotalBytes = 512 * 1024 // aggregate ceiling across one turn
)

// attachedFilesMarker introduces the attachment manifest inside a persisted
// user message. serializeEvents splits on it to keep raw file dumps out of
// the chat UI, so the marker must stay in sync between writer and reader.
const attachedFilesMarker = "\n\n[Attached files]"

// buildUserMessage assembles the framework message for one chat turn. The
// manifest lists every attachment with its staged workspace path and its
// source object; small text-like attachments within the size budget are
// additionally inlined as fenced blocks. Attachments must have been through
// normalizeAttachments first so Size is trustworthy. Bytes fetched here are
// kept on the AttachmentRef (Data) so staging reuses them instead of
// re-downloading.
func (s *service) buildUserMessage(
	ctx context.Context,
	client storage.StorageClient,
	content string,
	attachments []agentmod.AttachmentRef,
) model.Message {
	msg := model.Message{Role: model.RoleUser, Content: content}
	if len(attachments) == 0 {
		return msg
	}

	stagedPaths := agentmod.StagedInputPaths(attachments)
	var manifest, inlined []string
	remaining := maxInlineTotalBytes
	for i := range attachments {
		a := &attachments[i]
		if a.Unavailable {
			manifest = append(manifest, fmt.Sprintf(
				"- %s → UNAVAILABLE (object s3://%s/%s was not found in storage; do not reference it)",
				a.Name, a.Bucket, a.Key))
			continue
		}
		block := s.inlineAttachment(ctx, client, a, &remaining)
		note := ""
		if block != "" {
			inlined = append(inlined, block)
			note = " (contents inlined below)"
		}
		staged := a.StagedPath
		if staged == "" {
			staged = stagedPaths[i]
		}
		manifest = append(manifest, fmt.Sprintf("- %s → %s (source: s3://%s/%s)%s",
			a.Name, staged, a.Bucket, a.Key, note))
	}

	var b strings.Builder
	b.WriteString(content)
	b.WriteString(attachedFilesMarker)
	b.WriteString(" Each file below has been staged into your workspace at the listed path — read it from there with code. If a listed file is missing on disk, staging failed: tell the user instead of guessing or fabricating data. Small text files are also shown inline below.\n")
	b.WriteString(strings.Join(manifest, "\n"))
	for _, block := range inlined {
		b.WriteString("\n\n")
		b.WriteString(block)
	}
	msg.Content = b.String()
	return msg
}

// inlineAttachment returns the fenced block to embed for a single small
// text-like attachment, or "" when the file is not eligible (binary, too
// large, fetch error, over the per-turn byte budget). remaining tracks the
// shared aggregate budget and is decremented on success. Ineligible files are
// still staged into the sandbox; only the inline copy is skipped. Fetched
// bytes are stored on a.Data so the staging path can reuse them.
func (s *service) inlineAttachment(
	ctx context.Context,
	client storage.StorageClient,
	a *agentmod.AttachmentRef,
	remaining *int,
) string {
	if client == nil || !isTextLikeAttachment(a.Name, a.MimeType) {
		return ""
	}
	// Size has been verified against the real object by normalizeAttachments,
	// so these gates genuinely bound the GetObject read below.
	if a.Size <= 0 || a.Size > maxInlineFileBytes || int(a.Size) > *remaining {
		return ""
	}

	data, err := client.GetObject(ctx, storage.GetObjectRequest{Bucket: a.Bucket, Key: a.Key})
	if err != nil {
		log.L().Warn("inline attachment: fetch failed (staged only)",
			zap.String("bucket", a.Bucket), zap.String("key", a.Key), zap.Error(err))
		return ""
	}
	if len(data) > maxInlineFileBytes || len(data) > *remaining {
		return ""
	}
	// Keep the bytes for the staging path even when the content turns out to
	// be binary — staging uploads them verbatim either way.
	a.Data = data
	if !looksLikeText(data) {
		return ""
	}

	*remaining -= len(data)
	return renderInlineBlock(a.Name, a.MimeType, data)
}

// textExtensions is the fallback allowlist used when an attachment carries no
// MIME type or a generic octet-stream type. The post-fetch looksLikeText
// guard still rejects anything that turns out to be binary.
var textExtensions = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".rmd": true,
	".csv": true, ".tsv": true, ".json": true, ".ndjson": true, ".jsonl": true,
	".yaml": true, ".yml": true, ".xml": true, ".html": true, ".htm": true,
	".log": true, ".toml": true, ".ini": true, ".cfg": true, ".conf": true,
	".config": true, ".properties": true, ".env": true,
	".sh": true, ".bash": true, ".py": true, ".r": true, ".sql": true,
	".nf": true, ".groovy": true, ".tex": true,
	".gff": true, ".gff3": true, ".gtf": true, ".bed": true, ".sam": true,
	".vcf": true, ".fasta": true, ".fa": true, ".fai": true,
}

// isTextLikeAttachment decides whether an attachment is a candidate for
// inlining, based on its declared MIME type and, when that is absent or
// generic, its filename extension.
func isTextLikeAttachment(name, mime string) bool {
	m := strings.ToLower(strings.TrimSpace(mime))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i]) // drop "; charset=..."
	}
	if strings.HasPrefix(m, "text/") {
		return true
	}
	switch m {
	case "application/json", "application/xml", "application/x-yaml",
		"application/yaml", "application/x-ndjson", "application/ndjson",
		"application/csv", "application/javascript", "application/x-sh",
		"application/toml", "application/x-toml":
		return true
	}
	if m == "" || m == "application/octet-stream" {
		return textExtensions[strings.ToLower(path.Ext(name))]
	}
	return false
}

// looksLikeText rejects content that is not valid UTF-8 or contains a NUL
// byte — a cheap, reliable signal that a file is binary despite a text-like
// name or MIME type.
func looksLikeText(data []byte) bool {
	return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

// renderInlineBlock formats one attachment as a labelled fenced code block.
// The fence is widened past the longest backtick run in the body so file
// contents that themselves contain ``` cannot break out of the block.
func renderInlineBlock(name, mime string, data []byte) string {
	body := string(data)
	fence := chooseFence(body)
	var b strings.Builder
	fmt.Fprintf(&b, "===== FILE: %s (%s) =====\n", name, describeType(mime, len(data)))
	b.WriteString(fence)
	b.WriteString(fenceLang(name))
	b.WriteByte('\n')
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(fence)
	b.WriteByte('\n')
	return b.String()
}

// chooseFence returns a run of backticks at least one longer than the longest
// backtick run in body (minimum three), so the fence always encloses content.
func chooseFence(body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	n := max(longest+1, 3)
	return strings.Repeat("`", n)
}

// fenceLang maps a filename extension to a syntax-highlight hint for the
// opening fence. Unknown extensions yield "" (a plain fence).
func fenceLang(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".json", ".ndjson", ".jsonl":
		return "json"
	case ".csv":
		return "csv"
	case ".tsv":
		return "tsv"
	case ".yaml", ".yml":
		return "yaml"
	case ".md", ".markdown", ".rmd":
		return "markdown"
	case ".py":
		return "python"
	case ".r":
		return "r"
	case ".sh", ".bash":
		return "bash"
	case ".xml":
		return "xml"
	case ".html", ".htm":
		return "html"
	case ".sql":
		return "sql"
	case ".toml":
		return "toml"
	case ".nf", ".groovy":
		return "groovy"
	}
	return ""
}

// describeType renders the parenthetical label next to an inlined filename,
// e.g. "text/csv, 12.3 KB" (or just the size when the MIME type is unknown).
func describeType(mime string, size int) string {
	if strings.TrimSpace(mime) == "" {
		return humanSize(size)
	}
	return fmt.Sprintf("%s, %s", mime, humanSize(size))
}

// humanSize formats a byte count with a binary (1024-based) unit suffix.
func humanSize(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := int64(n) / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// serializeEvents reduces the framework's event stream to the typed
// message list the chat UI expects. The wire shape mirrors the live SSE
// events (see services/agent/stream.go) so MessageBubble.vue can render
// reloaded history identically to a fresh stream:
//
//	user / assistant         chat text
//	reasoning                assistant thinking block (foldable, muted in UI)
//	tool_call / tool_result  tool invocation + output
//	code_exec / code_result  workspace code execution
//	artifact                 saved artifacts (synthesised from
//	                         run_python_inline / workspace_save_artifact
//	                         tool results AND the end-of-turn harvest event,
//	                         so images and download links survive a reload)
//	skill_loaded             skill load pill (synthesised from skill_load
//	                         / skill_select_docs tool calls)
func serializeEvents(sess *session.Session) []gin.H {
	events := sess.GetEvents()
	out := make([]gin.H, 0, len(events))
	for i := range events {
		e := &events[i]
		if e.Response == nil || len(e.Response.Choices) == 0 {
			continue
		}

		// End-of-turn harvest event: rebuild one artifact card per saved
		// output file so reloaded history matches the live stream. The
		// event's assistant Content (a short summary, model-facing only) is
		// deliberately NOT rendered here.
		if arts, ok := agentmod.ParseHarvested(e); ok {
			for _, a := range arts {
				out = append(out, gin.H{
					"role":       "artifact",
					"name":       a.Name,
					"saved_as":   a.SavedAs,
					"version":    a.Version,
					"mime_type":  a.MimeType,
					"size_bytes": a.SizeBytes,
					"timestamp":  e.Timestamp,
				})
			}
			continue
		}

		// Code execution lifecycle events use Tag, not Role/ToolCalls.
		switch e.Tag {
		case event.CodeExecutionTag:
			payload := gin.H{
				"role":      "code_exec",
				"language":  "python",
				"timestamp": e.Timestamp,
			}
			if c := e.Response.Choices[0].Message.Content; c != "" {
				payload["code"] = c
			}
			out = append(out, payload)
			continue
		case event.CodeExecutionResultTag:
			out = append(out, gin.H{
				"role":      "code_result",
				"stdout":    e.Response.Choices[0].Message.Content,
				"timestamp": e.Timestamp,
			})
			continue
		}

		choice := e.Response.Choices[0]

		// Reasoning ("thinking") text rides on the same assistant message as the
		// answer / tool call. Emit it as a standalone entry first so reloaded
		// history shows the foldable thinking block ahead of its answer, matching
		// the live 'reasoning' SSE event ordering.
		if rc := choice.Message.ReasoningContent; rc != "" {
			out = append(out, gin.H{
				"role":      "reasoning",
				"content":   rc,
				"timestamp": e.Timestamp,
			})
		}

		switch {
		case len(choice.Message.ToolCalls) > 0:
			// One tool_call entry per call so the wire shape matches the
			// live SSE 'tool_call' event. Skill loads also get a higher-
			// level 'skill_loaded' pill, mirroring stream.go.
			for _, tc := range choice.Message.ToolCalls {
				out = append(out, gin.H{
					"role":      "tool_call",
					"tool_id":   tc.ID,
					"tool_name": tc.Function.Name,
					"arguments": string(tc.Function.Arguments),
					"timestamp": e.Timestamp,
				})
				if isSkillLoadCallName(tc.Function.Name) {
					out = append(out, gin.H{
						"role":      "skill_loaded",
						"name":      tc.Function.Name,
						"arguments": string(tc.Function.Arguments),
						"timestamp": e.Timestamp,
					})
				}
			}
		case choice.Message.Role == model.RoleTool:
			// Parallel tool calls are merged by the framework into a single
			// event with one choice per result (see
			// mergeParallelToolCallResponseEvents in trpc-agent-go). Iterate
			// every tool-result choice — not just Choices[0] — so a reloaded
			// conversation shows all of them, matching the live stream.
			for ci := range e.Response.Choices {
				msg := e.Response.Choices[ci].Message
				if msg.Role != model.RoleTool {
					continue
				}
				out = append(out, gin.H{
					"role":      "tool_result",
					"tool_id":   msg.ToolID,
					"tool_name": msg.ToolName,
					"content":   msg.Content,
					"timestamp": e.Timestamp,
				})
				// Synthesise artifact entries from artifact-producing tool
				// results so reloaded history shows the same image/file
				// previews the live stream did.
				for _, art := range parseArtifactsFromToolResult(
					msg.ToolName, msg.Content,
				) {
					if _, has := art["timestamp"]; !has {
						art["timestamp"] = e.Timestamp
					}
					art["role"] = "artifact"
					out = append(out, art)
				}
			}
		case choice.Message.Content != "":
			entry := gin.H{
				"role":      string(choice.Message.Role),
				"content":   choice.Message.Content,
				"timestamp": e.Timestamp,
			}
			// User messages persist the attachment manifest + inlined file
			// bodies (the model needs them on history replay), but the chat
			// UI should show the typed text plus attachment chips — not a
			// raw multi-hundred-KB file dump inside the bubble.
			if choice.Message.Role == model.RoleUser {
				if text, names := splitUserAttachments(choice.Message.Content); len(names) > 0 {
					entry["content"] = text
					atts := make([]gin.H, 0, len(names))
					for _, n := range names {
						atts = append(atts, gin.H{"name": n, "key": n})
					}
					entry["attachments"] = atts
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// splitUserAttachments separates a persisted user message into the text the
// user actually typed and the attachment names from the manifest that
// buildUserMessage appended after attachedFilesMarker. Returns the original
// content untouched when no marker is present. The manifest is a contiguous
// run of "- name → path" lines ending at the first blank line, so inlined
// file bodies (which follow after blank lines) can never be misparsed as
// manifest entries.
func splitUserAttachments(content string) (text string, names []string) {
	before, after, ok := strings.Cut(content, attachedFilesMarker)
	if !ok {
		return content, nil
	}
	text = strings.TrimRight(before, "\n")

	rest := after
	if nl := strings.Index(rest, "\n"); nl >= 0 {
		rest = rest[nl+1:] // drop the remainder of the marker/intro line
	} else {
		return text, nil
	}
	for line := range strings.SplitSeq(rest, "\n") {
		if strings.TrimSpace(line) == "" {
			break // end of the manifest block
		}
		entry, ok := strings.CutPrefix(line, "- ")
		if !ok {
			continue
		}
		if arrow := strings.Index(entry, " → "); arrow > 0 {
			names = append(names, entry[:arrow])
		}
	}
	return text, names
}

// parseArtifactsFromToolResult mirrors stream.go's emitArtifactEvents so
// the same artifact card shapes show up both on the live stream and on a
// reloaded conversation. Returns nil for tool results that don't carry
// artifacts.
func parseArtifactsFromToolResult(toolName, contentJSON string) []gin.H {
	switch toolName {
	case "run_python_inline":
		var res struct {
			Artifacts []map[string]any `json:"artifacts"`
		}
		if err := json.Unmarshal([]byte(contentJSON), &res); err != nil {
			return nil
		}
		out := make([]gin.H, 0, len(res.Artifacts))
		for _, a := range res.Artifacts {
			out = append(out, gin.H(a))
		}
		return out
	case "workspace_save_artifact", "antelope_save_artifact":
		var res map[string]any
		if err := json.Unmarshal([]byte(contentJSON), &res); err != nil {
			return nil
		}
		return []gin.H{gin.H(res)}
	}
	return nil
}

// isSkillLoadCallName mirrors stream.go's isSkillLoadCall — kept in this
// package so chat.go doesn't depend on the stream module.
func isSkillLoadCallName(name string) bool {
	switch strings.TrimSpace(name) {
	case "skill_load", "skill_select_docs":
		return true
	}
	return false
}

// silence unused import linter when the file is built standalone.
var _ = errors.New
