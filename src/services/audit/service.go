// Package audit records who did what to which storage, at the application
// layer.
//
// It exists because storage credentials are shared. A group grant lets every
// member act through one access key, so the object store's own access logs
// attribute all of it to the key's owner and cannot distinguish the people
// behind it. AnTelOpe is the only place that knows which human made the
// request and which grant permitted it, so attribution has to be written here
// or it does not exist at all.
package audit

import (
	"context"
	"time"

	"antelope/internal/modules/log"
	"antelope/models"
	"antelope/pkg/apperr"
	"antelope/pkg/response"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Event is one storage action to record. The recorder fills in the actor's
// email and the timestamp; callers supply what only they know.
type Event struct {
	ActorID         uint
	Action          string
	StorageConfigID uint
	Bucket          string
	ObjectKey       string
	Outcome         string
	DenyReason      string

	// ViaGroupID and ViaGroupName come from the authorization decision, which
	// already names the group whose grant matched.
	ViaGroupID   *uint
	ViaGroupName string
}

// Filter narrows a read of the log.
type Filter struct {
	ActorID         uint
	StorageConfigID uint
	Outcome         string
	From            *time.Time
	To              *time.Time
	Page            int
	PageSize        int
}

// MaxPageSize bounds a single read. The table grows with every browse action,
// so an unbounded query is a way to take the server down by asking politely.
const MaxPageSize = 200

const defaultPageSize = 50

// Recorder writes and reads the storage audit trail.
type Recorder interface {
	// RecordStorage persists one event. It deliberately returns nothing: see
	// the implementation for why a failure here must not surface.
	RecordStorage(ctx context.Context, ev Event)

	// ListStorage reads the trail back, newest first, with the total matching
	// count for pagination.
	ListStorage(ctx context.Context, f Filter) ([]models.StorageAuditEvent, int64, error)
}

type recorder struct {
	db *gorm.DB
}

func NewRecorder(db *gorm.DB) Recorder {
	return &recorder{db: db}
}

// RecordStorage writes the event and swallows any failure.
//
// The trade-off is deliberate. Losing an audit line is bad, but failing the
// user's storage request because the audit table is unavailable is worse: it
// converts a logging outage into a platform outage, and turns the audit table
// into a single point of failure for every browse and upload. So the error is
// logged loudly and the request proceeds. If the log must instead be
// authoritative enough to block on, that is a different product decision and
// needs a durable queue behind it, not a bare insert on the request path.
func (r *recorder) RecordStorage(ctx context.Context, ev Event) {
	if r == nil || r.db == nil {
		return
	}

	// Detach from the request's cancellation. A denied attempt is exactly the
	// case where the client may have already given up and disconnected, and
	// that is the record least worth losing.
	ctx = context.WithoutCancel(ctx)

	row := models.StorageAuditEvent{
		CreatedAt:       time.Now(),
		ActorID:         ev.ActorID,
		ActorEmail:      r.emailFor(ctx, ev.ActorID),
		Action:          ev.Action,
		StorageConfigID: ev.StorageConfigID,
		Bucket:          ev.Bucket,
		ObjectKey:       ev.ObjectKey,
		Outcome:         ev.Outcome,
		DenyReason:      ev.DenyReason,
		ViaGroupID:      ev.ViaGroupID,
		ViaGroupName:    ev.ViaGroupName,
	}

	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		// Loud on purpose: a silent gap in an audit trail is worse than a
		// noisy one, because nothing else will reveal that it happened.
		log.L().Error("AUDIT WRITE FAILED - storage action not recorded",
			zap.Uint("actorID", ev.ActorID),
			zap.String("action", ev.Action),
			zap.Uint("storageConfigID", ev.StorageConfigID),
			zap.String("bucket", ev.Bucket),
			zap.String("outcome", ev.Outcome),
			zap.Error(err))
	}
}

// emailFor resolves the actor's email for denormalization. A miss is not fatal:
// an event with an id and no email is still far better than no event.
func (r *recorder) emailFor(ctx context.Context, userID uint) string {
	if userID == 0 {
		return ""
	}
	var email string
	err := r.db.WithContext(ctx).
		Model(&models.User{}).
		Where("id = ?", userID).
		Limit(1).
		Pluck("email", &email).Error
	if err != nil {
		log.L().Warn("audit: could not resolve actor email",
			zap.Uint("actorID", userID), zap.Error(err))
	}
	return email
}

func (r *recorder) ListStorage(ctx context.Context, f Filter) ([]models.StorageAuditEvent, int64, error) {
	q := r.db.WithContext(ctx).Model(&models.StorageAuditEvent{})

	if f.ActorID != 0 {
		q = q.Where("actor_id = ?", f.ActorID)
	}
	if f.StorageConfigID != 0 {
		q = q.Where("storage_config_id = ?", f.StorageConfigID)
	}
	if f.Outcome != "" {
		q = q.Where("outcome = ?", f.Outcome)
	}
	if f.From != nil {
		q = q.Where("created_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("created_at <= ?", *f.To)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		log.L().Error("failed to count audit events", zap.Error(err))
		return nil, 0, apperr.ServerError(response.SystemError)
	}

	size := f.PageSize
	switch {
	case size <= 0:
		size = defaultPageSize
	case size > MaxPageSize:
		size = MaxPageSize
	}
	page := f.Page
	if page < 1 {
		page = 1
	}

	var events []models.StorageAuditEvent
	err := q.Order("created_at DESC, id DESC").
		Offset((page - 1) * size).
		Limit(size).
		Find(&events).Error
	if err != nil {
		log.L().Error("failed to list audit events", zap.Error(err))
		return nil, 0, apperr.ServerError(response.SystemError)
	}
	return events, total, nil
}
