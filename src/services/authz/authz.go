// Package authz is the single entry point for resource-level authorization.
//
// Every caller goes through the Authorizer interface. The policy engine behind
// it (currently Casbin) is an implementation detail: services and middleware
// only ever ask "can this user do this to that", never how the answer is
// reached. Swapping engines should touch this package and nothing else.
//
// Layering note: route-level role checks (super/admin/user) still live in
// routers/middleware and are unchanged. This package answers the orthogonal
// question of which specific resources a user may act on.
package authz

import (
	"context"
	"fmt"

	nixstorage "antelope/internal/modules/storage"
)

// Action is the verb being attempted. The set mirrors storage grant levels so
// a grant projects onto actions without translation.
type Action string

const (
	ActionRead  Action = "read"
	ActionWrite Action = "write"
	ActionAdmin Action = "admin"
)

// ResourceKind namespaces resource IDs so a storage config and a future
// pipeline resource with the same numeric ID never collide in policy.
type ResourceKind string

const (
	KindStorage ResourceKind = "storage"
)

// Resource identifies one governed object.
type Resource struct {
	Kind ResourceKind
	ID   uint
}

// StorageResource is shorthand for the only resource kind enforced today.
func StorageResource(configID uint) Resource {
	return Resource{Kind: KindStorage, ID: configID}
}

// String renders the policy object token, e.g. "storage:15".
func (r Resource) String() string {
	return fmt.Sprintf("%s:%d", r.Kind, r.ID)
}

// SubjectUser renders the policy subject token for a user.
func SubjectUser(userID uint) string {
	return fmt.Sprintf("user:%d", userID)
}

// SubjectGroup renders the policy subject token for a group.
func SubjectGroup(groupID uint) string {
	return fmt.Sprintf("group:%d", groupID)
}

// DenyReason classifies why access was refused, so the API can turn a denial
// into something the user can act on rather than an opaque 403.
type DenyReason string

const (
	// DenyNoGrant means nobody has granted this user access by any path.
	DenyNoGrant DenyReason = "no_grant"
	// DenyInsufficientLevel means the user can reach the resource but not with
	// this verb — typically read access attempting a write.
	DenyInsufficientLevel DenyReason = "insufficient_level"
	// DenyNotFound means the resource does not exist.
	DenyNotFound DenyReason = "not_found"
)

// GroupRef is the minimum a denial needs to tell the user who to ask.
type GroupRef struct {
	ID          uint   `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// OwnerEmails lists the group owners who can grant access. Empty when the
	// group has members but no designated owner.
	OwnerEmails []string `json:"owner_emails,omitempty"`
}

// Decision is an explained authorization result.
type Decision struct {
	Allowed bool `json:"allowed"`

	// Reason is set only when Allowed is false.
	Reason DenyReason `json:"reason,omitempty"`

	// GrantedVia names the group that produced an allow, empty for personal
	// configs and super-user bypasses.
	GrantedVia *GroupRef `json:"granted_via,omitempty"`

	// RequestAccessFrom lists groups already holding the resource, so the UI
	// can point the user at someone who can grant it.
	RequestAccessFrom []GroupRef `json:"request_access_from,omitempty"`

	// SuperBypass records that this was allowed by platform role rather than
	// by any grant. Always accompanied by an audit log entry.
	SuperBypass bool `json:"super_bypass,omitempty"`
}

// Authorizer is the only authorization surface callers touch.
type Authorizer interface {
	// Can reports whether userID may perform action on res.
	Can(ctx context.Context, userID uint, action Action, res Resource) (bool, error)

	// Explain answers the same question but returns why, including who to ask
	// when the answer is no.
	Explain(ctx context.Context, userID uint, action Action, res Resource) (Decision, error)

	// AccessibleStorageIDs lists every storage config the user may act on with
	// the given action. Used to populate pickers without an N+1 of Can calls.
	AccessibleStorageIDs(ctx context.Context, userID uint, action Action) ([]nixstorage.ConfigID, error)

	// ReloadPolicy rebuilds the in-memory policy from the domain tables.
	ReloadPolicy(ctx context.Context) error
}
