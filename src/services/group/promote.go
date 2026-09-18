package group

import (
	"context"
	"fmt"

	"antelope/internal/modules/log"
	nixstorage "antelope/internal/modules/storage"
	"antelope/models"
	"antelope/pkg/apperr"
	"antelope/pkg/response"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// PromotionCandidate is a set of personal configs pointing at one backend.
//
// Before group storage existed there was no way to share a bucket except for
// every member to configure it individually, so these clusters are what shared
// lab storage looks like in the old model.
type PromotionCandidate struct {
	Endpoint string            `json:"endpoint"`
	Configs  []CandidateConfig `json:"configs"`
	// SharedCredentials is true when every config in the cluster hashes the
	// same, meaning one credential was handed around rather than each user
	// being issued their own.
	SharedCredentials bool `json:"shared_credentials"`
}

// CandidateConfig is one personal config in a cluster, with its owner.
type CandidateConfig struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	OwnerID   uint   `json:"owner_id"`
	OwnerName string `json:"owner_name"`
	OwnerMail string `json:"owner_email"`
}

// PromoteInput describes a promotion.
type PromoteInput struct {
	StorageConfigID nixstorage.ConfigID `json:"storage_config_id"`
	GroupID         uint                `json:"group_id"`
	Classification  string              `json:"classification"`
	Name            string              `json:"name"`
	AccessLevel     string              `json:"access_level"`
}

// PromotionPreview reports what a promotion would do before it is committed.
type PromotionPreview struct {
	ConfigName string `json:"config_name"`
	GroupName  string `json:"group_name"`

	// DuplicatesRetired lists personal configs that will be removed because
	// they point at the same backend.
	DuplicatesRetired []CandidateConfig `json:"duplicates_retired"`

	// LosingAccess lists owners of those duplicates who are not in the target
	// group. They can reach this storage today and will not be able to
	// afterwards — the single most important thing to see before committing.
	LosingAccess []CandidateConfig `json:"losing_access"`

	// PersonalStorageConflict warns that promoting above "internal" will stop
	// these users writing this data to their own storage.
	RaisesClassification bool `json:"raises_classification"`
}

// PromotionResult summarises a completed promotion.
type PromotionResult struct {
	StorageConfigID   uint `json:"storage_config_id"`
	GroupID           uint `json:"group_id"`
	DuplicatesRetired int  `json:"duplicates_retired"`
	MembersAffected   int  `json:"members_affected"`
}

// ListPromotionCandidates surfaces shared buckets hiding as personal configs.
func (s *service) ListPromotionCandidates(ctx context.Context) ([]PromotionCandidate, error) {
	clusters, err := s.storage.FindPersonalDuplicates()
	if err != nil {
		log.L().Error("failed to scan for duplicate storage configs", zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}

	out := make([]PromotionCandidate, 0, len(clusters))
	for _, c := range clusters {
		configs := s.describeConfigs(ctx, c.Configs)

		shared := true
		for _, cfg := range c.Configs {
			if cfg.Hash != c.Configs[0].Hash {
				shared = false
				break
			}
		}

		out = append(out, PromotionCandidate{
			Endpoint:          c.Endpoint,
			Configs:           configs,
			SharedCredentials: shared,
		})
	}
	return out, nil
}

// describeConfigs attaches owner identity to a set of configs.
func (s *service) describeConfigs(ctx context.Context, configs []nixstorage.StorageConfig) []CandidateConfig {
	ownerIDs := make([]uint, 0, len(configs))
	for _, c := range configs {
		ownerIDs = append(ownerIDs, c.OwnerID)
	}

	var users []models.User
	if len(ownerIDs) > 0 {
		if err := s.db.WithContext(ctx).Where("id IN ?", ownerIDs).Find(&users).Error; err != nil {
			log.L().Warn("failed to resolve storage config owners", zap.Error(err))
		}
	}
	byID := make(map[uint]models.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}

	out := make([]CandidateConfig, 0, len(configs))
	for _, c := range configs {
		item := CandidateConfig{
			ID:      uint(c.ID),
			Name:    c.Name,
			OwnerID: c.OwnerID,
		}
		if u, ok := byID[c.OwnerID]; ok {
			item.OwnerName = u.Name
			item.OwnerMail = u.Email
		}
		out = append(out, item)
	}
	return out
}

// PreviewPromotion computes the effects of a promotion without applying it.
func (s *service) PreviewPromotion(ctx context.Context, in PromoteInput) (*PromotionPreview, error) {
	cfg, group, duplicates, err := s.promotionContext(ctx, in)
	if err != nil {
		return nil, err
	}

	described := s.describeConfigs(ctx, duplicates)
	losing := s.ownersOutsideGroup(ctx, s.describeConfigs(ctx, strippedOwners(cfg, duplicates)), in.GroupID)

	return &PromotionPreview{
		ConfigName:           cfg.Name,
		GroupName:            firstNonEmpty(group.DisplayName, group.Name),
		DuplicatesRetired:    described,
		LosingAccess:         losing,
		RaisesClassification: models.Classification(in.Classification).AtLeast(models.ClassificationRestricted),
	}, nil
}

// promotionContext loads and validates everything a promotion touches.
func (s *service) promotionContext(ctx context.Context, in PromoteInput) (
	*nixstorage.StorageConfig, *models.Group, []nixstorage.StorageConfig, error,
) {
	cfg, err := s.storage.GetConfig(in.StorageConfigID)
	if err != nil {
		return nil, nil, nil, apperr.ServerError(response.SystemError)
	}
	if cfg == nil {
		return nil, nil, nil, apperr.NotFound("Storage configuration not found")
	}

	var group models.Group
	if err := s.db.WithContext(ctx).First(&group, in.GroupID).Error; err != nil {
		return nil, nil, nil, apperr.NotFound("Group not found")
	}

	classification := models.Classification(in.Classification)
	if classification == "" {
		classification = models.Classification(cfg.Classification)
	}
	if !models.ValidClassification(classification) {
		return nil, nil, nil, apperr.CheckFail(response.CheckFailCode, "Unknown classification")
	}
	if !group.Clearance.AtLeast(classification) {
		return nil, nil, nil, apperr.Forbidden(fmt.Sprintf(
			"%q is cleared for %q data, which is below the %q classification you selected.",
			firstNonEmpty(group.DisplayName, group.Name), group.Clearance, classification))
	}

	matches, err := s.storage.FindConfigsMatching(cfg.Hash, cfg.Endpoint, cfg.ID)
	if err != nil {
		return nil, nil, nil, apperr.ServerError(response.SystemError)
	}

	duplicates := make([]nixstorage.StorageConfig, 0, len(matches))
	for _, m := range matches {
		if m.OwnerType == nixstorage.OwnerTypePersonal {
			duplicates = append(duplicates, m)
		}
	}
	return cfg, &group, duplicates, nil
}

// strippedOwners lists every config whose owner loses their personal route to
// the bucket. That is the retired duplicates plus the promoted config itself:
// its owner keeps the row but no longer owns it, so a non-member ends up cut
// off exactly like the duplicate owners. Callers pick the first config in the
// cluster by default, which makes this the likeliest case rather than an edge
// one. A config that is already group-owned has no personal owner to strip.
func strippedOwners(cfg *nixstorage.StorageConfig, duplicates []nixstorage.StorageConfig) []nixstorage.StorageConfig {
	out := make([]nixstorage.StorageConfig, 0, len(duplicates)+1)
	out = append(out, duplicates...)
	if cfg.OwnerType == nixstorage.OwnerTypePersonal {
		out = append(out, *cfg)
	}
	return out
}

// ownersOutsideGroup filters to config owners who are not group members.
func (s *service) ownersOutsideGroup(ctx context.Context, configs []CandidateConfig, groupID uint) []CandidateConfig {
	if len(configs) == 0 {
		return nil
	}

	ownerIDs := make([]uint, 0, len(configs))
	for _, c := range configs {
		ownerIDs = append(ownerIDs, c.OwnerID)
	}

	var memberIDs []uint
	if err := s.db.WithContext(ctx).
		Model(&models.GroupMembership{}).
		Where("group_id = ? AND user_id IN ?", groupID, ownerIDs).
		Pluck("user_id", &memberIDs).Error; err != nil {
		log.L().Warn("failed to check group membership for promotion preview", zap.Error(err))
		return nil
	}

	members := make(map[uint]bool, len(memberIDs))
	for _, id := range memberIDs {
		members[id] = true
	}

	losing := make([]CandidateConfig, 0)
	for _, c := range configs {
		if !members[c.OwnerID] {
			losing = append(losing, c)
		}
	}
	return losing
}

// PromoteConfig turns a personal config into group-owned storage.
//
// Retiring the duplicate personal copies is not cleanup, it is the point: a
// personal config capped at "internal" still pointing at a bucket now
// classified PHI would be an unlabelled route straight into controlled data,
// and the cap would be worth nothing.
func (s *service) PromoteConfig(ctx context.Context, actorID uint, in PromoteInput) (*PromotionResult, error) {
	cfg, group, duplicates, err := s.promotionContext(ctx, in)
	if err != nil {
		return nil, err
	}

	classification := in.Classification
	if classification == "" {
		classification = cfg.Classification
	}

	level := in.AccessLevel
	if models.ImpliedAccess(level) == nil {
		level = models.AccessWrite
	}

	name := in.Name
	if name == "" {
		name = firstNonEmpty(group.DisplayName, group.Name) + " storage"
	}

	// Re-parenting, granting, and retiring the duplicates are one atomic step.
	// A partial apply here is the dangerous state: a config marked PHI with the
	// personal copies still pointing at it is strictly worse than not having
	// promoted it at all.
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.storage.ReparentConfigTx(tx, cfg.ID, nixstorage.OwnerTypeGroup, group.ID, classification, name); err != nil {
			return err
		}

		grant := models.StorageGrant{
			StorageConfigID: uint(cfg.ID),
			GroupID:         group.ID,
			AccessLevel:     level,
			GrantedBy:       &actorID,
		}
		if err := tx.Where("storage_config_id = ? AND group_id = ?", cfg.ID, group.ID).
			Assign(map[string]any{"access_level": level, "granted_by": actorID}).
			FirstOrCreate(&grant).Error; err != nil {
			return err
		}

		for _, dup := range duplicates {
			if err := s.storage.DeleteConfigTx(tx, dup.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.L().Error("storage promotion failed",
			zap.Uint("configID", uint(in.StorageConfigID)), zap.Uint("groupID", in.GroupID), zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}

	// Caches are evicted only now the change is durable.
	s.storage.Evict(cfg.ID)
	for _, dup := range duplicates {
		s.storage.Evict(dup.ID)
	}

	s.syncPolicy(ctx)

	losing := s.ownersOutsideGroup(ctx, s.describeConfigs(ctx, strippedOwners(cfg, duplicates)), in.GroupID)

	log.L().Info("storage config promoted to group ownership",
		zap.Uint("configID", uint(cfg.ID)),
		zap.Uint("groupID", group.ID),
		zap.String("classification", classification),
		zap.Int("duplicatesRetired", len(duplicates)),
		zap.Int("ownersLosingAccess", len(losing)),
		zap.Uint("actorID", actorID))

	return &PromotionResult{
		StorageConfigID:   uint(cfg.ID),
		GroupID:           group.ID,
		DuplicatesRetired: len(duplicates),
		MembersAffected:   len(losing),
	}, nil
}
