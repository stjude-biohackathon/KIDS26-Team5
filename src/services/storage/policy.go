package storage

import (
	"context"
	"fmt"

	nixstorage "antelope/internal/modules/storage"
	"antelope/models"

	"gorm.io/gorm"
)

// personalStoragePolicy decides whether a user may register storage of their
// own, and what they are allowed to point it at.
//
// Personal storage is the one path by which data can leave governed storage: a
// user holding a grant on a restricted bucket could otherwise copy it into a
// bucket nobody classified. Two independent limits apply, and both have to
// pass.
type personalStoragePolicy struct {
	db              *gorm.DB
	platformAllowed bool
}

// PersonalDenial explains why personal storage is unavailable, so the UI can
// say something better than "forbidden".
type PersonalDenial struct {
	Allowed bool
	// BlockedByGroup names the group whose policy forbids it, when that is the
	// reason. Empty when the platform setting is the reason.
	BlockedByGroup string
	Message        string
}

// mayRegisterPersonal composes the platform setting with every group the user
// belongs to, most-restrictive-wins: membership in a single group that forbids
// personal storage removes the ability everywhere.
//
// That is deliberately blunt — joining one PHI lab costs a user personal
// storage on every project. Predictability matters more than precision in the
// rule that keeps controlled data from being copied somewhere ungoverned.
func (p personalStoragePolicy) mayRegisterPersonal(ctx context.Context, userID uint) (PersonalDenial, error) {
	if !p.platformAllowed {
		return PersonalDenial{
			Allowed: false,
			Message: "Personal storage is disabled on this deployment. Use storage shared with one of your groups.",
		}, nil
	}

	var blocking []models.Group
	err := p.db.WithContext(ctx).
		Model(&models.Group{}).
		Joins("JOIN group_memberships gm ON gm.group_id = groups.id AND gm.deleted_at IS NULL").
		Where("gm.user_id = ? AND groups.allow_personal_storage = ?", userID, false).
		Find(&blocking).Error
	if err != nil {
		return PersonalDenial{}, fmt.Errorf("storage: check personal storage policy: %w", err)
	}

	if len(blocking) > 0 {
		name := blocking[0].DisplayName
		if name == "" {
			name = blocking[0].Name
		}
		return PersonalDenial{
			Allowed:        false,
			BlockedByGroup: name,
			Message: fmt.Sprintf(
				"Your membership in %q does not permit personal storage. Use storage shared with your group.", name),
		}, nil
	}

	return PersonalDenial{Allowed: true}, nil
}

// assertNotGroupManaged blocks registering a personal config that points at a
// backend a group already manages.
//
// Without this the classification cap is decorative: a user could register a
// personal config — capped at "internal" — aimed straight at a bucket a group
// has classified as PHI, producing an unlabelled route into controlled data.
func (p personalStoragePolicy) assertNotGroupManaged(
	ctx context.Context,
	manager *nixstorage.ClientManager,
	hash, endpoint string,
) error {
	matches, err := manager.FindConfigsMatching(hash, endpoint, 0)
	if err != nil {
		return err
	}

	for _, m := range matches {
		if m.OwnerType != nixstorage.OwnerTypeGroup {
			continue
		}

		var group models.Group
		if err := p.db.WithContext(ctx).First(&group, m.OwnerID).Error; err != nil {
			// Cannot name the group, but the block still stands: the safe
			// answer when we know a group manages this backend is no.
			return fmt.Errorf("this storage is already managed by a group; request access instead")
		}
		name := group.DisplayName
		if name == "" {
			name = group.Name
		}
		return fmt.Errorf("this storage is managed by %q — request access from that group instead of configuring it personally", name)
	}
	return nil
}

// assertWriteTargetAllowed blocks copying restricted data into personal
// storage.
//
// Applied when a job reads from one config and writes to another: a personal
// config capped at "internal" must not become the destination for data a group
// classified as restricted or PHI.
func assertWriteTargetAllowed(readFrom, writeTo *nixstorage.StorageConfig) error {
	if readFrom == nil || writeTo == nil {
		return nil
	}
	if writeTo.OwnerType != nixstorage.OwnerTypePersonal {
		return nil
	}

	source := models.Classification(readFrom.Classification)
	if source.AtLeast(models.ClassificationRestricted) {
		return fmt.Errorf(
			"cannot write %s data to personal storage; choose a destination owned by a group cleared for it",
			readFrom.Classification)
	}
	return nil
}
