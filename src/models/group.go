package models

import "gorm.io/gorm"

// Classification ranks the sensitivity of the data a storage config holds.
// It cannot be inferred from a connection string, so it is always asserted by
// an administrator rather than derived.
type Classification string

const (
	ClassificationPublic     Classification = "public"
	ClassificationInternal   Classification = "internal"
	ClassificationRestricted Classification = "restricted"
	ClassificationPHI        Classification = "phi"
)

// classificationRank orders the tiers for comparison. Unknown values rank
// highest so a typo fails closed rather than silently granting access.
func classificationRank(c Classification) int {
	switch c {
	case ClassificationPublic:
		return 0
	case ClassificationInternal:
		return 1
	case ClassificationRestricted:
		return 2
	case ClassificationPHI:
		return 3
	default:
		return 99
	}
}

// ValidClassification reports whether c is one of the known tiers.
func ValidClassification(c Classification) bool {
	return classificationRank(c) != 99
}

// AtLeast reports whether c is at or above other in sensitivity.
func (c Classification) AtLeast(other Classification) bool {
	return classificationRank(c) >= classificationRank(other)
}

// MaxPersonalClassification caps what a user may self-register. Personal
// storage is an exfiltration path for anything above this: a user holding a
// grant on restricted data could otherwise copy it into a bucket nobody
// governs. Promotion to a higher tier requires an administrator.
const MaxPersonalClassification = ClassificationInternal

// GroupSource records where a group and its membership came from, so an Entra
// sync can later own its own rows without disturbing locally-managed ones.
type GroupSource string

const (
	GroupSourceLocal GroupSource = "local"
	GroupSourceEntra GroupSource = "entra"
)

// Role a member holds inside a group. This is scoped to the group and is
// distinct from User.Role, which remains the platform-wide role.
const (
	GroupRoleOwner  = "owner"
	GroupRoleMember = "member"
)

// Group is the unit that storage configurations are granted to. Membership in
// a group is what confers access; users never hold storage credentials
// directly except for their own personal configs.
//
// ExternalID and Source are unused while groups are managed locally. They ship
// now so inheriting membership from Entra later is a mapping exercise rather
// than a schema migration on a populated table.
type Group struct {
	gorm.Model

	// No index tag: this table soft-deletes, so uniqueness on name is a partial
	// index created in Migrate. A tag index would count deleted groups and make
	// their names permanently unusable. That partial index also serves name
	// lookups, since every query GORM scopes adds deleted_at IS NULL anyway.
	Name        string `gorm:"type:varchar(64);not null;comment:'stable slug used in policy subjects'" json:"name"`
	DisplayName string `gorm:"type:varchar(128);comment:'human label shown in the UI'" json:"display_name"`
	Description string `gorm:"type:varchar(512);comment:'what this group is for'" json:"description,omitempty"`

	// Clearance caps the classification of storage this group may be granted.
	Clearance Classification `gorm:"type:varchar(20);not null;default:'internal';comment:'highest data tier this group may hold'" json:"clearance"`

	// CostCenter ties the group to a grant or budget line so compute spend can
	// be rolled up to whoever is funding it.
	CostCenter string `gorm:"type:varchar(64);comment:'grant or cost centre code for spend rollup'" json:"cost_center,omitempty"`

	// AllowPersonalStorage gates whether members may register their own
	// storage at all. Composed most-restrictive-wins across every group a user
	// belongs to: one group forbidding it removes the ability everywhere.
	AllowPersonalStorage bool `gorm:"not null;default:true;comment:'may members register personal storage configs'" json:"allow_personal_storage"`

	ExternalID string      `gorm:"type:varchar(256);index;comment:'idp object id; empty for local groups'" json:"external_id,omitempty"`
	Source     GroupSource `gorm:"type:varchar(20);not null;default:'local';comment:'local | entra'" json:"source"`

	Memberships []GroupMembership `gorm:"constraint:OnDelete:CASCADE;" json:"memberships,omitempty"`
}

// GroupMembership joins a user to a group. Deliberately a real table rather
// than relying on the policy engine's own grouping rows: the UI needs display
// names, timestamps, and who added whom, none of which a policy tuple carries.
type GroupMembership struct {
	gorm.Model

	// Uniqueness of (group_id, user_id) is enforced by a partial index created
	// in Migrate rather than a tag here: this table soft-deletes, and a tag
	// index would count tombstones, making a removed member impossible to add
	// back. See enforceLiveUniqueness.
	GroupID     uint        `gorm:"index;not null;comment:'group id'" json:"group_id"`
	UserID      uint        `gorm:"index;not null;comment:'member user id'" json:"user_id"`
	RoleInGroup string      `gorm:"type:varchar(20);not null;default:'member';comment:'owner | member'" json:"role_in_group"`
	AddedBy     *uint       `gorm:"comment:'user who added this member; null for seeded/synced rows'" json:"added_by,omitempty"`
	Source      GroupSource `gorm:"type:varchar(20);not null;default:'local';comment:'local | entra'" json:"source"`

	Group *Group `gorm:"foreignKey:GroupID" json:"group,omitempty"`
	User  *User  `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

// Access levels on a storage grant, ordered least to most capable.
const (
	AccessRead  = "read"
	AccessWrite = "write"
	AccessAdmin = "admin"
)

// ImpliedAccess expands a grant level into every level it confers. Grants are
// projected into the policy engine as explicit rows per action rather than
// resolved by a matcher at request time, which keeps a denial explainable by
// pointing at the row that was or was not there.
func ImpliedAccess(level string) []string {
	switch level {
	case AccessAdmin:
		return []string{AccessRead, AccessWrite, AccessAdmin}
	case AccessWrite:
		return []string{AccessRead, AccessWrite}
	case AccessRead:
		return []string{AccessRead}
	default:
		return nil
	}
}

// StorageGrant records that a group may use a storage configuration. The
// grant, not the credential, is what members receive: secrets stay in the
// config and are never shown to the people who use them.
type StorageGrant struct {
	gorm.Model

	// As with GroupMembership, uniqueness is a partial index created in Migrate
	// so a revoked grant does not block re-granting the same pair.
	StorageConfigID uint   `gorm:"index;not null;comment:'storage config being granted'" json:"storage_config_id"`
	GroupID         uint   `gorm:"index;not null;comment:'group receiving access'" json:"group_id"`
	AccessLevel     string `gorm:"type:varchar(20);not null;default:'read';comment:'read | write | admin'" json:"access_level"`

	// Prefix is unused today: grants cover the whole bucket. The column ships
	// now so narrowing a grant to a key prefix later needs no migration.
	Prefix string `gorm:"type:varchar(512);comment:'reserved: restrict grant to a key prefix'" json:"prefix,omitempty"`

	GrantedBy *uint `gorm:"comment:'user who created the grant'" json:"granted_by,omitempty"`

	Group *Group `gorm:"foreignKey:GroupID" json:"group,omitempty"`
}
