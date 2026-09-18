package storage

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"antelope/internal/modules/log"
	nixstorage "antelope/internal/modules/storage"
	"antelope/models"
	"antelope/pkg/apperr"
	"antelope/pkg/response"
	"antelope/pkg/types"
	"antelope/services/audit"
	"antelope/services/authz"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Service provides storage bucket/object and configuration operations.
//
// Every operation that touches a backend resolves a storage config first and
// authorizes it through authz. Which config is used is either named explicitly
// by the caller or defaulted; either way the user must hold access to it.
type Service interface {
	// Config
	ListConfigs(ctx context.Context, userID uint) (gin.H, error)
	GetConfig(ctx context.Context, userID uint) (gin.H, error)
	SaveConfig(ctx context.Context, userID uint, dto types.UserStorageConfigDto) error
	DeleteConfig(ctx context.Context, userID uint) error
	TestConnection(dto types.TestStorageConnectionDto) (gin.H, error)

	// ResolveConfigForUser picks the config a user should act on when they did
	// not name one, and authorizes the choice. Exported so job submission can
	// share exactly the same resolution and checks.
	ResolveConfigForUser(ctx context.Context, userID uint, requested nixstorage.ConfigID, action authz.Action) (*nixstorage.StorageConfig, error)

	// Bucket / object operations. configID may be 0, meaning "my default".
	GetBuckets(ctx context.Context, userID uint, configID nixstorage.ConfigID) (gin.H, error)
	GetObjects(ctx context.Context, userID uint, configID nixstorage.ConfigID, bucket, prefix string) (gin.H, error)
	GetUploadURL(ctx context.Context, userID uint, configID nixstorage.ConfigID, req types.PresignedURLReqDto) (gin.H, error)
	GetDownloadURL(ctx context.Context, userID uint, configID nixstorage.ConfigID, req types.PresignedURLReqDto) (gin.H, error)
	CreateBucket(ctx context.Context, userID uint, configID nixstorage.ConfigID, req types.CreateBucketReqDto) error
	DeleteBucket(ctx context.Context, userID uint, configID nixstorage.ConfigID, bucketName string) error
	DeleteObject(ctx context.Context, userID uint, configID nixstorage.ConfigID, bucket, prefix string, recursive bool) error
}

type ossService struct {
	manager *nixstorage.ClientManager
	authz   authz.Authorizer
	audit   audit.Recorder
	policy  personalStoragePolicy
}

func NewOssService(
	manager *nixstorage.ClientManager,
	authorizer authz.Authorizer,
	recorder audit.Recorder,
	db *gorm.DB,
	platformAllowsPersonal bool,
) Service {
	return &ossService{
		manager: manager,
		authz:   authorizer,
		audit:   recorder,
		policy: personalStoragePolicy{
			db:              db,
			platformAllowed: platformAllowsPersonal,
		},
	}
}

// ── Config resolution ──────────────────────────────────────────────────────

// ResolveConfigForUser turns an optional config id into a config the user is
// allowed to use with action.
//
// When requested is 0 the default is group storage over personal: a lab's
// shared bucket is the sanctioned destination, survives the member leaving, and
// keeps results together. Personal storage stays available but has to be asked
// for by name.
func (s *ossService) ResolveConfigForUser(
	ctx context.Context,
	userID uint,
	requested nixstorage.ConfigID,
	action authz.Action,
) (*nixstorage.StorageConfig, error) {
	cfg, _, err := s.resolve(ctx, userID, requested, action)
	return cfg, err
}

// resolve is ResolveConfigForUser plus the authorization decision behind it.
//
// The decision is what names the group whose grant permitted the action, so
// anything that audits needs it; the exported wrapper keeps the narrower
// signature for callers that only want the config.
func (s *ossService) resolve(
	ctx context.Context,
	userID uint,
	requested nixstorage.ConfigID,
	action authz.Action,
) (*nixstorage.StorageConfig, authz.Decision, error) {
	if s.manager == nil {
		return nil, authz.Decision{}, apperr.CheckFail(response.CheckFailCode, response.StorageNotConfigured)
	}

	if requested != 0 {
		cfg, err := s.manager.GetConfig(requested)
		if err != nil {
			log.L().Error("failed to load storage config", zap.Uint("configID", uint(requested)), zap.Error(err))
			return nil, authz.Decision{}, apperr.ServerError(response.SystemError)
		}
		if cfg == nil {
			// Worth auditing: naming a config that does not exist is what
			// probing for other people's storage ids looks like.
			return nil, authz.Decision{Reason: authz.DenyNotFound},
				apperr.NotFound("Storage configuration not found")
		}

		decision, err := s.decide(ctx, userID, action, requested)
		if err != nil {
			return nil, decision, err
		}
		if !decision.Allowed {
			return nil, decision, apperr.Forbidden(DenialMessage(decision))
		}
		return cfg, decision, nil
	}

	ids, err := s.authz.AccessibleStorageIDs(ctx, userID, action)
	if err != nil {
		log.L().Error("failed to list accessible storage", zap.Uint("userID", userID), zap.Error(err))
		return nil, authz.Decision{}, apperr.ServerError(response.SystemError)
	}
	if len(ids) == 0 {
		return nil, authz.Decision{}, apperr.CheckFail(response.CheckFailCode, response.StorageNotConfigured)
	}

	configs, err := s.manager.ListConfigsByIDs(ids)
	if err != nil {
		log.L().Error("failed to load accessible storage configs", zap.Error(err))
		return nil, authz.Decision{}, apperr.ServerError(response.SystemError)
	}
	if len(configs) == 0 {
		return nil, authz.Decision{}, apperr.CheckFail(response.CheckFailCode, response.StorageNotConfigured)
	}

	sort.Slice(configs, func(i, j int) bool {
		gi := configs[i].OwnerType == nixstorage.OwnerTypeGroup
		gj := configs[j].OwnerType == nixstorage.OwnerTypeGroup
		if gi != gj {
			return gi // group storage first
		}
		if configs[i].Name != configs[j].Name {
			return configs[i].Name < configs[j].Name
		}
		return configs[i].ID < configs[j].ID
	})

	cfg := &configs[0]
	// Already known to be allowed — this call is for the attribution, so the
	// audit row can say which grant the defaulted choice rests on.
	decision, err := s.decide(ctx, userID, action, cfg.ID)
	if err != nil {
		return nil, decision, err
	}
	return cfg, decision, nil
}

// decide runs the explained authorization check.
func (s *ossService) decide(
	ctx context.Context,
	userID uint,
	action authz.Action,
	configID nixstorage.ConfigID,
) (authz.Decision, error) {
	decision, err := s.authz.Explain(ctx, userID, action, authz.StorageResource(uint(configID)))
	if err != nil {
		log.L().Error("authorization check failed",
			zap.Uint("userID", userID), zap.Uint("configID", uint(configID)), zap.Error(err))
		return authz.Decision{}, apperr.ServerError(response.SystemError)
	}
	return decision, nil
}

// authorize turns a denial into an error carrying who to ask.
func (s *ossService) authorize(ctx context.Context, userID uint, action authz.Action, configID nixstorage.ConfigID) error {
	decision, err := s.decide(ctx, userID, action, configID)
	if err != nil {
		return err
	}
	if decision.Allowed {
		return nil
	}
	return apperr.Forbidden(DenialMessage(decision))
}

// auditTarget is what an operation is about to do, for the audit trail.
type auditTarget struct {
	action string
	bucket string
	key    string
}

// open resolves and authorizes a storage operation, records the attempt, and
// returns a client to act with.
//
// The row is written before the backend call runs, so what it attests is an
// authorized attempt, not a completed effect. That is the honest claim: a
// later failure in the object store is not something this layer observes
// reliably, and pretending otherwise would make the log say more than it knows.
func (s *ossService) open(
	ctx context.Context,
	userID uint,
	configID nixstorage.ConfigID,
	action authz.Action,
	target auditTarget,
) (nixstorage.StorageClient, error) {
	cfg, decision, err := s.resolve(ctx, userID, configID, action)
	if err != nil {
		// Attribute the denial to whichever config the attempt named. With no
		// config at all there is nothing to attribute it to, and the user
		// simply has no storage — not a denial worth a row.
		id := uint(configID)
		if cfg != nil {
			id = uint(cfg.ID)
		}
		if id != 0 {
			s.recordStorage(ctx, userID, id, target, decision)
		}
		return nil, err
	}

	client := s.manager.GetClient(cfg.ID)
	if client == nil {
		return nil, apperr.CheckFail(response.CheckFailCode, response.StorageNotConfigured)
	}

	s.recordStorage(ctx, userID, uint(cfg.ID), target, decision)
	return client, nil
}

// recordStorage translates an authorization decision into an audit event.
func (s *ossService) recordStorage(
	ctx context.Context,
	userID uint,
	configID uint,
	target auditTarget,
	decision authz.Decision,
) {
	if s.audit == nil {
		return
	}

	ev := audit.Event{
		ActorID:         userID,
		Action:          target.action,
		StorageConfigID: configID,
		Bucket:          target.bucket,
		ObjectKey:       target.key,
		Outcome:         models.AuditOutcomeAllowed,
	}
	if !decision.Allowed {
		ev.Outcome = models.AuditOutcomeDenied
		ev.DenyReason = string(decision.Reason)
	}
	// Null for personal storage and super-user bypasses, which is the correct
	// reading: no group conferred anything in those cases.
	if decision.GrantedVia != nil {
		id := decision.GrantedVia.ID
		ev.ViaGroupID = &id
		ev.ViaGroupName = decision.GrantedVia.DisplayName
		if ev.ViaGroupName == "" {
			ev.ViaGroupName = decision.GrantedVia.Name
		}
	}

	s.audit.RecordStorage(ctx, ev)
}

// ── Storage config operations ──────────────────────────────────────────────

// ListConfigs returns every config the user can read, with scope and
// classification so the UI can label them. Credentials are never included.
func (s *ossService) ListConfigs(ctx context.Context, userID uint) (gin.H, error) {
	if s.manager == nil {
		return gin.H{"configs": []types.StorageConfigSummary{}}, nil
	}

	ids, err := s.authz.AccessibleStorageIDs(ctx, userID, authz.ActionRead)
	if err != nil {
		log.L().Error("failed to list accessible storage", zap.Uint("userID", userID), zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}

	configs, err := s.manager.ListConfigsByIDs(ids)
	if err != nil {
		log.L().Error("failed to load storage configs", zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}

	writable, err := s.authz.AccessibleStorageIDs(ctx, userID, authz.ActionWrite)
	if err != nil {
		return nil, apperr.ServerError(response.SystemError)
	}
	canWrite := make(map[nixstorage.ConfigID]bool, len(writable))
	for _, id := range writable {
		canWrite[id] = true
	}

	groupNames := s.groupNamesFor(ctx, configs)
	sharedVia := s.grantingGroupsFor(ctx, userID, configs)

	summaries := make([]types.StorageConfigSummary, 0, len(configs))
	for _, c := range configs {
		summaries = append(summaries, types.StorageConfigSummary{
			ID:             uint(c.ID),
			Name:           c.Name,
			OwnerType:      c.OwnerType,
			OwnerName:      groupNames[c.ID],
			Classification: c.Classification,
			Endpoint:       c.Endpoint,
			Writable:       canWrite[c.ID],
			Owned:          c.OwnerType == nixstorage.OwnerTypePersonal && c.OwnerID == userID,
			SharedVia:      sharedVia[c.ID],
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		gi := summaries[i].OwnerType == nixstorage.OwnerTypeGroup
		gj := summaries[j].OwnerType == nixstorage.OwnerTypeGroup
		if gi != gj {
			return gi
		}
		return summaries[i].Name < summaries[j].Name
	})

	personal, _ := s.policy.mayRegisterPersonal(ctx, userID)
	return gin.H{
		"configs":                   summaries,
		"may_add_personal":          personal.Allowed,
		"personal_denied_reason":    personal.Message,
		"personal_blocked_by_group": personal.BlockedByGroup,
	}, nil
}

// grantingGroupsFor resolves, per config, the display names of the caller's
// groups that grant them access.
//
// A grant is independent of ownership — sharing a bucket someone already
// registered personally is the ordinary case — so the owner columns cannot
// explain where a member's access came from, and this is the only thing the UI
// can honestly show them.
func (s *ossService) grantingGroupsFor(ctx context.Context, userID uint, configs []nixstorage.StorageConfig) map[nixstorage.ConfigID][]string {
	via := make(map[nixstorage.ConfigID][]string, len(configs))
	if len(configs) == 0 || userID == 0 {
		return via
	}

	ids := make([]uint, 0, len(configs))
	for _, c := range configs {
		ids = append(ids, uint(c.ID))
	}

	var rows []struct {
		StorageConfigID uint
		Label           string
	}
	err := s.policy.db.WithContext(ctx).
		Table("storage_grants AS sg").
		Select("sg.storage_config_id AS storage_config_id, COALESCE(NULLIF(g.display_name, ''), g.name) AS label").
		Joins("JOIN groups g ON g.id = sg.group_id AND g.deleted_at IS NULL").
		Joins("JOIN group_memberships m ON m.group_id = sg.group_id AND m.user_id = ? AND m.deleted_at IS NULL", userID).
		Where("sg.deleted_at IS NULL AND sg.storage_config_id IN ?", ids).
		Scan(&rows).Error
	if err != nil {
		log.L().Warn("failed to resolve granting groups", zap.Uint("userID", userID), zap.Error(err))
		return via
	}

	for _, r := range rows {
		id := nixstorage.ConfigID(r.StorageConfigID)
		via[id] = append(via[id], r.Label)
	}
	return via
}

// groupNamesFor resolves display names for group-owned configs in one query.
func (s *ossService) groupNamesFor(ctx context.Context, configs []nixstorage.StorageConfig) map[nixstorage.ConfigID]string {
	names := make(map[nixstorage.ConfigID]string, len(configs))

	groupIDs := make([]uint, 0, len(configs))
	for _, c := range configs {
		if c.OwnerType == nixstorage.OwnerTypeGroup {
			groupIDs = append(groupIDs, c.OwnerID)
		}
	}
	if len(groupIDs) == 0 {
		return names
	}

	var groups []models.Group
	if err := s.policy.db.WithContext(ctx).Where("id IN ?", groupIDs).Find(&groups).Error; err != nil {
		return names
	}
	byID := make(map[uint]models.Group, len(groups))
	for _, g := range groups {
		byID[g.ID] = g
	}

	for _, c := range configs {
		if c.OwnerType != nixstorage.OwnerTypeGroup {
			continue
		}
		if g, ok := byID[c.OwnerID]; ok {
			if g.DisplayName != "" {
				names[c.ID] = g.DisplayName
			} else {
				names[c.ID] = g.Name
			}
		}
	}
	return names
}

// GetConfig returns the caller's personal config.
//
// Kept pointing at personal storage specifically so the existing settings page
// keeps behaving as it did: it is the form for "my own credentials", and group
// storage is not editable from there.
func (s *ossService) GetConfig(ctx context.Context, userID uint) (gin.H, error) {
	if s.manager == nil {
		return gin.H{"config": types.UserStorageConfigResponse{Configured: false}}, nil
	}

	rec, err := s.manager.PersonalConfigForUser(userID)
	if err != nil {
		log.L().Error("failed to look up personal storage config", zap.Uint("userID", userID), zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}
	if rec == nil {
		return gin.H{"config": types.UserStorageConfigResponse{Configured: false}}, nil
	}

	cfg, err := s.manager.GetProviderConfig(rec.ID)
	if err != nil || cfg == nil {
		log.L().Error("failed to get storage config", zap.Uint("userID", userID), zap.Error(err))
		return gin.H{"config": types.UserStorageConfigResponse{Configured: false}}, nil
	}

	var minioConfig nixstorage.MinioConfig
	if err := json.Unmarshal(cfg.RawConfig, &minioConfig); err != nil {
		log.L().Error("failed to unmarshal storage config", zap.Uint("userID", userID), zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}

	return gin.H{"config": types.UserStorageConfigResponse{
		ID:                 uint(rec.ID),
		Host:               minioConfig.Host,
		Port:               minioConfig.Port,
		AccessKey:          minioConfig.AccessKey,
		SecretKey:          maskString(minioConfig.SecretKey),
		UseSSL:             minioConfig.UseSSL,
		Region:             minioConfig.Region,
		InsecureSkipVerify: minioConfig.InsecureSkipVerify,
		Configured:         true,
	}}, nil
}

// SaveConfig creates or updates the caller's personal storage config.
func (s *ossService) SaveConfig(ctx context.Context, userID uint, dto types.UserStorageConfigDto) error {
	minioConfig := nixstorage.MinioConfig{
		Host:               dto.Host,
		Port:               dto.Port,
		AccessKey:          dto.AccessKey,
		SecretKey:          dto.SecretKey,
		UseSSL:             dto.UseSSL,
		Region:             dto.Region,
		InsecureSkipVerify: dto.InsecureSkipVerify,
	}

	rawConfig, err := json.Marshal(minioConfig)
	if err != nil {
		return apperr.ServerError(response.SystemError)
	}

	if s.manager == nil {
		if err := nixstorage.TestMinioConnection(minioConfig); err != nil {
			log.L().Error("storage connection test failed", zap.Uint("userID", userID), zap.Error(err))
			return apperr.CheckFail(response.CheckFailCode, "Failed to connect to storage: "+err.Error())
		}
		return nil
	}

	existing, err := s.manager.PersonalConfigForUser(userID)
	if err != nil {
		return apperr.ServerError(response.SystemError)
	}

	// Policy only gates creating personal storage, not updating credentials on
	// one that already exists — otherwise enabling the restriction would strand
	// users with a config they can no longer rotate.
	if existing == nil {
		denial, err := s.policy.mayRegisterPersonal(ctx, userID)
		if err != nil {
			return apperr.ServerError(response.SystemError)
		}
		if !denial.Allowed {
			return apperr.Forbidden(denial.Message)
		}
	}

	client, envelope, configJSON, err := s.manager.ValidateConfig(nixstorage.ProviderMinio, json.RawMessage(rawConfig))
	if err != nil {
		log.L().Error("storage connection test failed", zap.Uint("userID", userID), zap.Error(err))
		return apperr.CheckFail(response.CheckFailCode, "Failed to connect to storage: "+err.Error())
	}
	_ = client

	endpoint := nixstorage.EndpointOf(nixstorage.ProviderMinio, json.RawMessage(rawConfig))

	if err := s.policy.assertNotGroupManaged(ctx, s.manager, envelope.Hash, endpoint); err != nil {
		return apperr.Forbidden(err.Error())
	}

	if existing != nil {
		if _, err := s.manager.SetClient(existing.ID, nixstorage.ProviderMinio, json.RawMessage(rawConfig)); err != nil {
			return apperr.CheckFail(response.CheckFailCode, "Failed to connect to storage: "+err.Error())
		}
		log.L().Info("personal storage configuration updated",
			zap.Uint("userID", userID), zap.String("host", dto.Host))
		return nil
	}

	// Personal storage is capped: it is the one destination the platform does
	// not govern, so it must never be the label on controlled data.
	rec, err := s.manager.CreateConfig(nixstorage.ConfigSpec{
		Name:           "Personal",
		OwnerType:      nixstorage.OwnerTypePersonal,
		OwnerID:        userID,
		Classification: string(models.MaxPersonalClassification),
		CreatedBy:      userID,
	}, envelope, configJSON, endpoint)
	if err != nil {
		log.L().Error("failed to create personal storage config", zap.Uint("userID", userID), zap.Error(err))
		return apperr.ServerError(response.SystemError)
	}

	// New config means a new implicit self-grant; the policy must see it before
	// the user's next request.
	if err := s.authz.ReloadPolicy(ctx); err != nil {
		log.L().Error("failed to reload authz policy after storage create", zap.Error(err))
	}

	log.L().Info("personal storage configuration saved",
		zap.Uint("userID", userID), zap.Uint("configID", uint(rec.ID)), zap.String("host", dto.Host))
	return nil
}

// DeleteConfig removes the caller's personal storage config. Group storage is
// not reachable from here — it is removed by an administrator, not by a member.
func (s *ossService) DeleteConfig(ctx context.Context, userID uint) error {
	if s.manager == nil {
		return nil
	}

	rec, err := s.manager.PersonalConfigForUser(userID)
	if err != nil {
		return apperr.ServerError(response.SystemError)
	}
	if rec == nil {
		return nil
	}

	if err := s.manager.RemoveClient(rec.ID); err != nil {
		log.L().Error("failed to delete storage config", zap.Uint("userID", userID), zap.Error(err))
		return apperr.ServerError(response.SystemError)
	}

	if err := s.authz.ReloadPolicy(ctx); err != nil {
		log.L().Error("failed to reload authz policy after storage delete", zap.Error(err))
	}

	log.L().Info("personal storage configuration deleted", zap.Uint("userID", userID))
	return nil
}

func (s *ossService) TestConnection(dto types.TestStorageConnectionDto) (gin.H, error) {
	testConfig := nixstorage.MinioConfig{
		Host:               dto.Host,
		Port:               dto.Port,
		AccessKey:          dto.AccessKey,
		SecretKey:          dto.SecretKey,
		UseSSL:             dto.UseSSL,
		Region:             dto.Region,
		InsecureSkipVerify: dto.InsecureSkipVerify,
	}
	if err := nixstorage.TestMinioConnection(testConfig); err != nil {
		return nil, apperr.CheckFail(response.CheckFailCode, "Connection failed: "+err.Error())
	}
	return gin.H{"success": true}, nil
}

// ── Bucket / object operations ─────────────────────────────────────────────

func (s *ossService) GetBuckets(ctx context.Context, userID uint, configID nixstorage.ConfigID) (gin.H, error) {
	client, err := s.open(ctx, userID, configID, authz.ActionRead,
		auditTarget{action: models.StorageActionListBuckets})
	if err != nil {
		return nil, err
	}

	buckets, err := client.ListBuckets(ctx)
	if err != nil {
		log.L().Error("list buckets failed", zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}

	bucketInfos := make([]types.BucketInfo, len(buckets))
	for i, b := range buckets {
		bucketInfos[i] = types.BucketInfo{Name: b.Name, CreationDate: b.CreatedAt}
	}
	return gin.H{"buckets": bucketInfos}, nil
}

func (s *ossService) GetObjects(ctx context.Context, userID uint, configID nixstorage.ConfigID, bucket, prefix string) (gin.H, error) {
	client, err := s.open(ctx, userID, configID, authz.ActionRead,
		auditTarget{action: models.StorageActionListObjects, bucket: bucket, key: prefix})
	if err != nil {
		return nil, err
	}

	if exists, err := client.BucketExists(ctx, bucket); err != nil {
		log.L().Error("check bucket existence failed", zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	} else if !exists {
		return nil, apperr.NotFound(response.BucketNotFound)
	}

	objectCh, err := client.ListObjects(ctx, bucket, prefix, false)
	if err != nil {
		log.L().Error("list objects failed", zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}

	var objects []types.ObjectInfo
	for entry := range objectCh {
		if entry.Err != nil {
			log.L().Error("get object failed", zap.String("bucket", bucket), zap.String("object", entry.Key), zap.Error(entry.Err))
			return nil, apperr.ServerError(response.SystemError)
		}
		if entry.Key == prefix {
			continue
		}
		relativeName := strings.TrimPrefix(entry.Key, prefix)
		if relativeName == "" {
			continue
		}
		if entry.IsDir {
			if folderName := strings.TrimSuffix(relativeName, "/"); folderName != "" {
				objects = append(objects, types.ObjectInfo{Name: folderName, IsFolder: true})
			}
		} else {
			objects = append(objects, types.ObjectInfo{
				Name:         relativeName,
				Size:         entry.Size,
				LastModified: entry.LastModified,
				IsFolder:     false,
				ContentType:  entry.ContentType,
			})
		}
	}
	return gin.H{"objects": objects}, nil
}

// GetUploadURL mints a presigned PUT, so it requires write access — the URL
// grants the bearer the ability to write whether or not they come back here.
func (s *ossService) GetUploadURL(ctx context.Context, userID uint, configID nixstorage.ConfigID, req types.PresignedURLReqDto) (gin.H, error) {
	// Only the bucket and key are recorded. The URL itself is a bearer
	// credential — anyone holding it can write without authenticating — so it
	// must never reach the audit table or the logs.
	client, err := s.open(ctx, userID, configID, authz.ActionWrite,
		auditTarget{action: models.StorageActionUploadURLIssued, bucket: req.Bucket, key: req.Key})
	if err != nil {
		return nil, err
	}
	uploadURL, err := client.PresignedPutObject(ctx, req.Bucket, req.Key, 24*time.Hour)
	if err != nil {
		log.L().Error("get object upload url failed", zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}
	return gin.H{"urls": types.PresignedURLRespDto{UploadURL: uploadURL}}, nil
}

// GetDownloadURL mints a presigned GET. The audit row says a download URL was
// issued, not that a download happened: the browser fetches the object from S3
// directly, so the transfer is invisible to this server and may never occur.
func (s *ossService) GetDownloadURL(ctx context.Context, userID uint, configID nixstorage.ConfigID, req types.PresignedURLReqDto) (gin.H, error) {
	client, err := s.open(ctx, userID, configID, authz.ActionRead,
		auditTarget{action: models.StorageActionDownloadURLIssued, bucket: req.Bucket, key: req.Key})
	if err != nil {
		return nil, err
	}
	downloadURL, err := client.PresignedGetObject(ctx, req.Bucket, req.Key, 24*time.Hour)
	if err != nil {
		log.L().Error("get object download url failed", zap.Error(err))
		return nil, apperr.ServerError(response.SystemError)
	}
	return gin.H{"urls": types.PresignedURLRespDto{DownloadURL: downloadURL}}, nil
}

func (s *ossService) CreateBucket(ctx context.Context, userID uint, configID nixstorage.ConfigID, req types.CreateBucketReqDto) error {
	client, err := s.open(ctx, userID, configID, authz.ActionWrite,
		auditTarget{action: models.StorageActionCreateBucket, bucket: req.Name})
	if err != nil {
		return err
	}
	if err := client.CreateBucket(ctx, req.Name); err != nil {
		log.L().Error("create bucket failed", zap.Error(err))
		return apperr.ServerError(response.SystemError)
	}
	return nil
}

// DeleteBucket destroys a whole bucket, so it requires admin on the config
// rather than write — a member with write access to shared lab storage should
// be able to add results, not remove the bucket holding everyone's.
func (s *ossService) DeleteBucket(ctx context.Context, userID uint, configID nixstorage.ConfigID, bucketName string) error {
	client, err := s.open(ctx, userID, configID, authz.ActionAdmin,
		auditTarget{action: models.StorageActionDeleteBucket, bucket: bucketName})
	if err != nil {
		return err
	}
	if err := client.RemoveBucket(ctx, bucketName); err != nil {
		log.L().Error("delete bucket failed", zap.Error(err))
		return apperr.ServerError(response.SystemError)
	}
	return nil
}

func (s *ossService) DeleteObject(ctx context.Context, userID uint, configID nixstorage.ConfigID, bucket, prefix string, recursive bool) error {
	client, err := s.open(ctx, userID, configID, authz.ActionWrite,
		auditTarget{action: models.StorageActionDeleteObject, bucket: bucket, key: prefix})
	if err != nil {
		return err
	}

	if recursive && strings.HasSuffix(prefix, "/") {
		listCh, err := client.ListObjects(ctx, bucket, prefix, true)
		if err != nil {
			log.L().Error("list objects for deletion failed", zap.Error(err))
			return apperr.ServerError(response.ObjectDeleteError)
		}

		objectsCh := make(chan nixstorage.ObjectEntry)
		go func() {
			defer close(objectsCh)
			for entry := range listCh {
				if entry.Err != nil {
					log.L().Error("list object failed", zap.Error(entry.Err))
					continue
				}
				select {
				case objectsCh <- entry:
				case <-ctx.Done():
					// RemoveObjects stopped consuming (e.g. the loop below
					// returned on first error); stop feeding rather than
					// blocking this goroutine until ctx is torn down.
					return
				}
			}
		}()

		for rErr := range client.RemoveObjects(ctx, bucket, objectsCh) {
			if rErr.Err != nil {
				log.L().Error("remove object failed", zap.Error(rErr.Err))
				return apperr.ServerError(response.ObjectDeleteError)
			}
		}
		return nil
	}

	if err := client.DeleteObject(ctx, bucket, prefix); err != nil {
		log.L().Error("remove object failed", zap.Error(err))
		return apperr.ServerError(response.ObjectDeleteError)
	}
	return nil
}

func maskString(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}
