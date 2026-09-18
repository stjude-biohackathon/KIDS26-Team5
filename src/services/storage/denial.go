package storage

import (
	"fmt"
	"strings"

	"antelope/services/authz"
)

// DenialMessage turns an authorization decision into something the user can act
// on.
//
// A bare "forbidden" tells a researcher nothing and turns into a support
// ticket. Naming the group that already holds the storage, and whoever owns
// that group, converts the dead end into a request they can make themselves.
func DenialMessage(d authz.Decision) string {
	if d.Allowed {
		return ""
	}

	switch d.Reason {
	case authz.DenyNotFound:
		return "That storage configuration does not exist."

	case authz.DenyInsufficientLevel:
		if who := requestTarget(d.RequestAccessFrom); who != "" {
			return fmt.Sprintf(
				"You have read-only access to this storage. Ask %s for write access.", who)
		}
		return "You have read-only access to this storage."

	default:
		if who := requestTarget(d.RequestAccessFrom); who != "" {
			return fmt.Sprintf("You do not have access to this storage. Request access from %s.", who)
		}
		return "You do not have access to this storage. Ask an administrator to grant it to one of your groups."
	}
}

// requestTarget renders the most useful "ask this person" string available:
// a named owner if the group has one, otherwise the group itself.
func requestTarget(groups []authz.GroupRef) string {
	if len(groups) == 0 {
		return ""
	}

	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		label := g.DisplayName
		if label == "" {
			label = g.Name
		}
		if len(g.OwnerEmails) > 0 {
			parts = append(parts, fmt.Sprintf("%s (%s)", label, strings.Join(g.OwnerEmails, ", ")))
			continue
		}
		parts = append(parts, label)
	}

	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
}
