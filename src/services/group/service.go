// Package group owns groups, their membership, and the storage grants that
// hang off them.
//
// Every mutation here follows the same shape: change the domain tables inside a
// transaction, then rebuild the authorization policy from them. The domain
// tables are the only source of truth — the policy engine holds nothing of its
// own — so the two cannot drift apart. A stale rule left behind after a
// revocation would be a silent security hole, and this ordering makes that
// state unrepresentable rather than merely unlikely.
package group

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"antelope/internal/modules/log"
	nixstorage "antelope/internal/modules/storage"
	"antelope/models"
	"antelope/pkg/apperr"
	"antelope/pkg/response"
	"antelope/services/authz"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Service manages groups, membership, and storage grants.
type Service interface {
	ListGroups(ctx context.Context) ([]GroupDetail, error)
	GetGroup(ctx context.Context, groupID uint) (*GroupDetail, error)
	CreateGroup(ctx context.Context, actorID uint, in GroupInput) (*models.Group, error)
	UpdateGroup(ctx context.Context, groupID uint, in GroupInput) error
	DeleteGroup(ctx context.Context, groupID uint) error

	AddMember(ctx context.Context, actorID, groupID, userID uint, roleInGroup string) error
	RemoveMember(ctx context.Context, groupID, userID uint) error

	GrantStorage(ctx context.Context, actorID uint, in GrantInput) error
	RevokeStorage(ctx context.Context, configID nixstorage.ConfigID, groupID uint) error

	ListPromotionCandidates(ctx context.Context) ([]PromotionCandidate, error)
	PreviewPromotion(ctx context.Context, in PromoteInput) (*PromotionPreview, error)
	PromoteConfig(ctx context.Context, actorID uint, in PromoteInput) (*PromotionResult, error)
}

// GroupInput carries the mutable fields of a group.
type GroupInput struct {
	Name                 string `json:"name"`
	DisplayName          string `json:"display_name"`
	Description          string `json:"description"`
	Clearance            string `json:"clearance"`
	CostCenter           string `json:"cost_center"`
	AllowPersonalStorage *bool  `json:"allow_personal_storage"`
}

// MemberSummary is a member as the admin UI shows them.
type MemberSummary struct {
	UserID      uint   `json:"user_id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	RoleInGroup string `json:"role_in_group"`
}

// GrantSummary is one storage grant with enough context to display it.
type GrantSummary struct {
	StorageConfigID uint   `json:"storage_config_id"`
	ConfigName      string `json:"config_name"`
	Classification  string `json:"classification"`
	Endpoint        string `json:"endpoint,omitempty"`
	AccessLevel     string `json:"access_level"`
}

// GroupDetail is a group with its membership and grants.
type GroupDetail struct {
	models.Group
	Members []MemberSummary `json:"members"`
	Grants  []GrantSummary  `json:"grants"`
}

// GrantInput describes a grant to create or update.
type GrantInput struct {
	StorageConfigID nixstorage.ConfigID `json:"storage_config_id"`
	GroupID         uint                `json:"group_id"`
	AccessLevel     string              `json:"access_level"`
}

type service struct {
	db      *gorm.DB
	storage *nixstorage.ClientManager
	authz   authz.Authorizer
}

func NewService(db *gorm.DB, stor *nixstorage.ClientManager, authorizer authz.Authorizer) Service {
	return &service{db: db, storage: stor, authz: authorizer}
}

// syncPolicy rebuilds authorization from the domain tables. Called after every
// committed mutation; a failure is logged loudly because it means this replica
// is serving decisions from a policy that no longer matches the database.
func (s *service) syncPolicy(ctx context.Context) {
	if err := s.authz.ReloadPolicy(ctx); err != nil {
		log.L().Error("failed to reload authorization policy after group change; "+
			"this replica may authorize against stale rules until the next reload",
			zap.Error(err))
	}
}

// ── Groups ────────────────────────────────────────────────────────────────────

func (s *service) ListGroups(ctx context.Context) ([]GroupDetail, error) {
	var groups []models.Group
	if err := s.db.WithContext(ctx).Order("display_name ASC, name ASC").Find(&groups).Error; err != nil {
		return nil, apperr.ServerError(response.SystemError)
	}

	details := make([]GroupDetail, 0, len(groups))
	for i := range groups {
		d, err := s.detailFor(ctx, groups[i])
		if err != nil {
			return nil, err
		}
		details = append(details, *d)
	}
	return details, nil
}

func (s *service) GetGroup(ctx context.Context, groupID uint) (*GroupDetail, error) {
	var g models.Group
	if err := s.db.WithContext(ctx).First(&g, groupID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.NotFound("Group not found")
		}
		return nil, apperr.ServerError(response.SystemError)
	}
	return s.detailFor(ctx, g)
}

func (s *service) detailFor(ctx context.Context, g models.Group) (*GroupDetail, error) {
	var members []MemberSummary
	err := s.db.WithContext(ctx).
		Table("group_memberships gm").
		Select("gm.user_id, gm.role_in_group, users.name, users.email").
		Joins("JOIN users ON users.id = gm.user_id AND users.deleted_at IS NULL").
		Where("gm.group_id = ? AND gm.deleted_at IS NULL", g.ID).
		Order("gm.role_in_group = 'owner' DESC, users.name ASC").
		Scan(&members).Error
	if err != nil {
		return nil, apperr.ServerError(response.SystemError)
	}

	var grants []models.StorageGrant
	if err := s.db.WithContext(ctx).Where("group_id = ?", g.ID).Find(&grants).Error; err != nil {
		return nil, apperr.ServerError(response.SystemError)
	}

	summaries := make([]GrantSummary, 0, len(grants))
	for _, gr := range grants {
		item := GrantSummary{
			StorageConfigID: uint(gr.StorageConfigID),
			AccessLevel:     gr.AccessLevel,
		}
		if cfg, err := s.storage.GetConfig(nixstorage.ConfigID(gr.StorageConfigID)); err == nil && cfg != nil {
			item.ConfigName = cfg.Name
			item.Classification = cfg.Classification
			item.Endpoint = cfg.Endpoint
		}
		summaries = append(summaries, item)
	}

	return &GroupDetail{Group: g, Members: members, Grants: summaries}, nil
}

func (s *service) CreateGroup(ctx context.Context, actorID uint, in GroupInput) (*models.Group, error) {
	name := slug(in.Name)
	if name == "" {
		name = slug(in.DisplayName)
	}
	if name == "" {
		return nil, apperr.CheckFail(response.CheckFailCode, "Group name is required")
	}

	clearance := models.Classification(in.Clearance)
	if clearance == "" {
		clearance = models.ClassificationInternal
	}
	if !models.ValidClassification(clearance) {
		return nil, apperr.CheckFail(response.CheckFailCode, "Unknown clearance level")
	}

	allowPersonal := true
	if in.AllowPersonalStorage != nil {
		allowPersonal = *in.AllowPersonalStorage
	}

	g := models.Group{
		Name:                 name,
		DisplayName:          firstNonEmpty(in.DisplayName, in.Name),
		Description:          in.Description,
		Clearance:            clearance,
		CostCenter:           in.CostCenter,
		AllowPersonalStorage: allowPersonal,
		Source:               models.GroupSourceLocal,
	}
	if err := s.db.WithContext(ctx).Create(&g).Error; err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			return nil, apperr.CheckFail(response.CheckFailCode, "A group with that name already exists")
		}
		return nil, apperr.ServerError(response.SystemError)
	}

	log.L().Info("group created",
		zap.Uint("groupID", g.ID), zap.String("name", g.Name), zap.Uint("actorID", actorID))
	return &g, nil
}

func (s *service) UpdateGroup(ctx context.Context, groupID uint, in GroupInput) error {
	var g models.Group
	if err := s.db.WithContext(ctx).First(&g, groupID).Error; err != nil {
		return apperr.NotFound("Group not found")
	}

	updates := map[string]any{}
	if in.DisplayName != "" {
		updates["display_name"] = in.DisplayName
	}
	if in.Description != "" {
		updates["description"] = in.Description
	}
	if in.CostCenter != "" {
		updates["cost_center"] = in.CostCenter
	}
	if in.AllowPersonalStorage != nil {
		updates["allow_personal_storage"] = *in.AllowPersonalStorage
	}

	if in.Clearance != "" {
		clearance := models.Classification(in.Clearance)
		if !models.ValidClassification(clearance) {
			return apperr.CheckFail(response.CheckFailCode, "Unknown clearance level")
		}
		// Lowering clearance below storage the group already holds would leave
		// it with access it is no longer cleared for. Refuse rather than
		// silently orphan the grant.
		if err := s.assertClearanceCoversGrants(ctx, groupID, clearance); err != nil {
			return err
		}
		updates["clearance"] = clearance
	}

	if len(updates) == 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).Model(&models.Group{}).Where("id = ?", groupID).
		Updates(updates).Error; err != nil {
		return apperr.ServerError(response.SystemError)
	}
	return nil
}

// assertClearanceCoversGrants blocks a clearance change that would leave the
// group holding storage above its new level.
func (s *service) assertClearanceCoversGrants(ctx context.Context, groupID uint, clearance models.Classification) error {
	var grants []models.StorageGrant
	if err := s.db.WithContext(ctx).Where("group_id = ?", groupID).Find(&grants).Error; err != nil {
		return apperr.ServerError(response.SystemError)
	}
	for _, gr := range grants {
		cfg, err := s.storage.GetConfig(nixstorage.ConfigID(gr.StorageConfigID))
		if err != nil || cfg == nil {
			continue
		}
		if !clearance.AtLeast(models.Classification(cfg.Classification)) {
			return apperr.CheckFail(response.CheckFailCode, fmt.Sprintf(
				"Cannot lower clearance below %q: the group still holds %q storage. Revoke that grant first.",
				cfg.Classification, cfg.Name))
		}
	}
	return nil
}

func (s *service) DeleteGroup(ctx context.Context, groupID uint) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_id = ?", groupID).Delete(&models.StorageGrant{}).Error; err != nil {
			return err
		}
		if err := tx.Where("group_id = ?", groupID).Delete(&models.GroupMembership{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.Group{}, groupID).Error
	})
	if err != nil {
		log.L().Error("failed to delete group", zap.Uint("groupID", groupID), zap.Error(err))
		return apperr.ServerError(response.SystemError)
	}

	s.syncPolicy(ctx)
	log.L().Info("group deleted", zap.Uint("groupID", groupID))
	return nil
}

// ── Membership ────────────────────────────────────────────────────────────────

func (s *service) AddMember(ctx context.Context, actorID, groupID, userID uint, roleInGroup string) error {
	if roleInGroup != models.GroupRoleOwner {
		roleInGroup = models.GroupRoleMember
	}

	var g models.Group
	if err := s.db.WithContext(ctx).First(&g, groupID).Error; err != nil {
		return apperr.NotFound("Group not found")
	}
	var u models.User
	if err := s.db.WithContext(ctx).First(&u, userID).Error; err != nil {
		return apperr.NotFound("User not found")
	}

	membership := models.GroupMembership{
		GroupID:     groupID,
		UserID:      userID,
		RoleInGroup: roleInGroup,
		AddedBy:     &actorID,
		Source:      models.GroupSourceLocal,
	}
	err := s.db.WithContext(ctx).
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Assign(map[string]any{"role_in_group": roleInGroup}).
		FirstOrCreate(&membership).Error
	if err != nil {
		log.L().Error("failed to add group member",
			zap.Uint("groupID", groupID), zap.Uint("userID", userID), zap.Error(err))
		return apperr.ServerError(response.SystemError)
	}

	s.syncPolicy(ctx)
	log.L().Info("group member added",
		zap.Uint("groupID", groupID), zap.Uint("userID", userID),
		zap.String("role", roleInGroup), zap.Uint("actorID", actorID))
	return nil
}

func (s *service) RemoveMember(ctx context.Context, groupID, userID uint) error {
	err := s.db.WithContext(ctx).
		Where("group_id = ? AND user_id = ?", groupID, userID).
		Delete(&models.GroupMembership{}).Error
	if err != nil {
		return apperr.ServerError(response.SystemError)
	}

	// Access must stop now, not when the user's token expires — which is why
	// memberships are resolved server-side rather than carried in the JWT.
	s.syncPolicy(ctx)
	log.L().Info("group member removed",
		zap.Uint("groupID", groupID), zap.Uint("userID", userID))
	return nil
}

// ── Storage grants ────────────────────────────────────────────────────────────

func (s *service) GrantStorage(ctx context.Context, actorID uint, in GrantInput) error {
	level := in.AccessLevel
	if models.ImpliedAccess(level) == nil {
		return apperr.CheckFail(response.CheckFailCode, "Access level must be read, write, or admin")
	}

	cfg, err := s.storage.GetConfig(in.StorageConfigID)
	if err != nil {
		return apperr.ServerError(response.SystemError)
	}
	if cfg == nil {
		return apperr.NotFound("Storage configuration not found")
	}

	var g models.Group
	if err := s.db.WithContext(ctx).First(&g, in.GroupID).Error; err != nil {
		return apperr.NotFound("Group not found")
	}

	// A group may only hold storage at or below its clearance. This is the
	// check that makes classification mean something: without it, restricted
	// data could be handed to a group nobody vetted for it.
	if !g.Clearance.AtLeast(models.Classification(cfg.Classification)) {
		return apperr.Forbidden(fmt.Sprintf(
			"%q is cleared for %q data but this storage is classified %q. Raise the group's clearance first.",
			firstNonEmpty(g.DisplayName, g.Name), g.Clearance, cfg.Classification))
	}

	grant := models.StorageGrant{
		StorageConfigID: uint(in.StorageConfigID),
		GroupID:         in.GroupID,
		AccessLevel:     level,
		GrantedBy:       &actorID,
	}
	err = s.db.WithContext(ctx).
		Where("storage_config_id = ? AND group_id = ?", in.StorageConfigID, in.GroupID).
		Assign(map[string]any{"access_level": level, "granted_by": actorID}).
		FirstOrCreate(&grant).Error
	if err != nil {
		log.L().Error("failed to grant storage",
			zap.Uint("configID", uint(in.StorageConfigID)), zap.Uint("groupID", in.GroupID), zap.Error(err))
		return apperr.ServerError(response.SystemError)
	}

	s.syncPolicy(ctx)
	log.L().Info("storage granted to group",
		zap.Uint("configID", uint(in.StorageConfigID)), zap.Uint("groupID", in.GroupID),
		zap.String("level", level), zap.Uint("actorID", actorID))
	return nil
}

func (s *service) RevokeStorage(ctx context.Context, configID nixstorage.ConfigID, groupID uint) error {
	err := s.db.WithContext(ctx).
		Where("storage_config_id = ? AND group_id = ?", configID, groupID).
		Delete(&models.StorageGrant{}).Error
	if err != nil {
		return apperr.ServerError(response.SystemError)
	}

	s.syncPolicy(ctx)
	log.L().Info("storage grant revoked",
		zap.Uint("configID", uint(configID)), zap.Uint("groupID", groupID))
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func slug(label string) string {
	out := strings.ToLower(strings.TrimSpace(label))
	out = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, out)
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
