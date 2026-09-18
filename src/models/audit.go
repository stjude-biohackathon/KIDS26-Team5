package models

import "time"

// Storage audit actions.
//
// The presigned-URL actions are named "issued" on purpose. AnTelOpe mints a URL
// and hands it to the browser, which then talks to S3 directly — the transfer
// never passes through this server, so we cannot know whether the bytes moved,
// how many, or even whether the URL was used at all before it expired. Naming
// these "upload"/"download" would claim knowledge the platform does not have.
// What the row honestly attests is that this user was authorized to obtain a
// credential for this object at this time.
const (
	StorageActionListBuckets       = "storage.bucket.list"
	StorageActionCreateBucket      = "storage.bucket.create"
	StorageActionDeleteBucket      = "storage.bucket.delete"
	StorageActionListObjects       = "storage.object.list"
	StorageActionDeleteObject      = "storage.object.delete"
	StorageActionUploadURLIssued   = "storage.upload_url.issued"
	StorageActionDownloadURLIssued = "storage.download_url.issued"
)

// Audit outcomes.
const (
	AuditOutcomeAllowed = "allowed"
	AuditOutcomeDenied  = "denied"
)

// StorageAuditEvent records one attempt to use a storage configuration.
//
// This exists because a group shares a single credential. The bucket's own
// server-side logs therefore attribute every action to whoever owns that
// credential, and cannot tell one member from another. Attribution to a real
// person only exists at this layer, so if it is not recorded here it is not
// recorded anywhere.
//
// Deliberately not gorm.Model: an audit row is append-only. gorm.Model would
// add UpdatedAt and a soft-delete column, both of which imply this table can be
// rewritten, and a DeletedAt would let a delete quietly hide evidence while
// looking like an ordinary GORM call.
type StorageAuditEvent struct {
	ID        uint      `gorm:"primarykey" json:"id"`
	CreatedAt time.Time `gorm:"index;not null;comment:'when the attempt happened'" json:"created_at"`

	// ActorEmail is denormalized rather than joined at read time because
	// DeleteUser hard-deletes the row: without a copy here, every action a
	// departed user took would become unattributable, which is precisely when
	// an audit trail is most likely to be needed.
	ActorID    uint   `gorm:"index;not null;comment:'user who attempted the action'" json:"actor_id"`
	ActorEmail string `gorm:"type:varchar(256);comment:'actor email as it was at the time'" json:"actor_email"`

	Action          string `gorm:"type:varchar(48);index;not null;comment:'storage.* action attempted'" json:"action"`
	StorageConfigID uint   `gorm:"index;not null;comment:'storage config acted on'" json:"storage_config_id"`

	Bucket    string `gorm:"type:varchar(256);comment:'bucket, when the action names one'" json:"bucket,omitempty"`
	ObjectKey string `gorm:"type:varchar(1024);comment:'object key or prefix, when applicable'" json:"object_key,omitempty"`

	Outcome string `gorm:"type:varchar(16);index;not null;comment:'allowed | denied'" json:"outcome"`
	// DenyReason carries the authorizer's machine-readable reason, so denials
	// can be separated into "no access at all" and "had read, wanted write".
	DenyReason string `gorm:"type:varchar(64);comment:'authz reason when denied'" json:"deny_reason,omitempty"`

	// ViaGroupID names the group whose grant conferred the access. This is the
	// RBAC-relevant fact and the reason the whole table is interesting: it
	// answers "why was this allowed" rather than only "who did it". Null when
	// the user reached the config as its personal owner or as a super user.
	ViaGroupID   *uint  `gorm:"index;comment:'group whose grant conferred access; null if not via a group'" json:"via_group_id,omitempty"`
	ViaGroupName string `gorm:"type:varchar(128);comment:'group display name at the time'" json:"via_group_name,omitempty"`
}
