package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"antelope/internal/modules/log"
	"antelope/internal/modules/sse"
	nixstorage "antelope/internal/modules/storage"
	"antelope/models"
	"antelope/pkg/apperr"
	"antelope/pkg/response"
	"antelope/pkg/types"
	"antelope/services/authz"

	nomad "github.com/hashicorp/nomad/api"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// NomadJobsAPI is the narrow interface over Nomad's Jobs API used by this service.
type NomadJobsAPI interface {
	Dispatch(jobID string, meta map[string]string, payload []byte, idPfx string, q *nomad.WriteOptions) (*nomad.JobDispatchResponse, *nomad.WriteMeta, error)
	Deregister(jobID string, purge bool, q *nomad.WriteOptions) (string, *nomad.WriteMeta, error)
}

// Config holds job service configuration.
type Config struct {
	Task string
}

// Service provides job management operations.
type Service interface {
	AddJob(dto types.JobAddDto) error
	DeleteJob(dto types.JobDeleteDto) error
	StopJob(dto types.JobStopDto) error
	GetUserJobs(userID uint, page, pageSize int) (map[string]any, error)
	GetJobDetails(jobID uint) (types.JobDetailDto, error)
	// GO-3: StreamJobLog no longer depends on *gin.Context; the handler passes
	// the raw http.ResponseWriter and request context instead.
	StreamJobLog(ctx context.Context, w http.ResponseWriter, allocID, logType string)
	CheckJobOwnership(userID, jobID uint) bool
	CheckAllocOwnership(userID uint, allocID string) bool
	// ReconcileStaleDispatches resolves jobs left in the pre-dispatch state by a
	// pod that died mid-dispatch. Intended to run once at startup.
	ReconcileStaleDispatches(olderThan time.Duration) (int64, error)
}

// StorageResolver is the slice of the storage service job submission needs:
// pick a config for a user and confirm they may write to it. Narrow interface
// rather than the whole service so the dependency stays legible.
type StorageResolver interface {
	ResolveConfigForUser(ctx context.Context, userID uint, requested nixstorage.ConfigID, action authz.Action) (*nixstorage.StorageConfig, error)
}

type jobService struct {
	db          *gorm.DB
	nomadJobs   NomadJobsAPI
	nomadClient *nomad.Client
	cfg         Config
	sse         *sse.Manager
	storage     *nixstorage.ClientManager
	storageSvc  StorageResolver
}

func NewService(db *gorm.DB, nomadJobs NomadJobsAPI, nomadClient *nomad.Client, cfg Config, sse *sse.Manager, storage *nixstorage.ClientManager, storageSvc StorageResolver) Service {
	return &jobService{
		db:          db,
		nomadJobs:   nomadJobs,
		nomadClient: nomadClient,
		cfg:         cfg,
		sse:         sse,
		storageSvc:  storageSvc,
		storage:     storage,
	}
}

func (s *jobService) CheckJobOwnership(userID, jobID uint) bool {
	return CheckJobOwnership(s.db, userID, jobID)
}

func (s *jobService) CheckAllocOwnership(userID uint, allocID string) bool {
	return CheckAllocOwnership(s.db, userID, allocID)
}

func (s *jobService) GetUserJobs(userID uint, page, pageSize int) (map[string]any, error) {
	var total int64
	var jobs []types.JobListDto

	// BUG-3: propagate Count errors instead of silently using zero
	if err := s.db.Model(&models.Job{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		log.L().Error("count user jobs failed", zap.Uint("user_id", userID), zap.Error(err))
		return nil, apperr.ServerError(err.Error())
	}

	pagination := s.db.Where("user_id = ?", userID).Limit(pageSize).Offset((page - 1) * pageSize)
	if err := pagination.Model(&models.Job{}).
		Select("id,pipeline_name,pipeline_version,created_at,status,dispatch_id,alloc_id").
		Order("created_at DESC").
		Scan(&jobs).Error; err != nil {
		log.L().Error("get user job list with pagination failed",
			zap.Uint("user_id", userID), zap.Error(err))
		return nil, apperr.ServerError(err.Error())
	}

	return map[string]any{
		"pagination": map[string]any{
			"page":        page,
			"page_size":   pageSize,
			"total":       total,
			"total_pages": int(math.Ceil(float64(total) / float64(pageSize))),
		},
		"jobs": jobs,
	}, nil
}

// nomadDispatchTimeout bounds the background Nomad dispatch HTTP call so a hung
// Nomad API cannot leave the detached dispatch goroutine running until pod
// restart. The dispatch endpoint accepts the job asynchronously, so the call
// should return in seconds; this is a generous safety ceiling.
const nomadDispatchTimeout = 60 * time.Second

func (s *jobService) AddJob(dto types.JobAddDto) error {
	ctx := context.Background()

	// Resolve which storage this run writes to and confirm the user may write
	// there. A run produces output, so read access is not enough.
	cfgRecord, err := s.storageSvc.ResolveConfigForUser(
		ctx, dto.UserId, nixstorage.ConfigID(dto.StorageConfigId), authz.ActionWrite)
	if err != nil {
		return err
	}

	storageConfig, err := s.minioConfigFor(cfgRecord.ID)
	if err != nil {
		log.L().Error("failed to load storage credentials",
			zap.Uint("userId", dto.UserId), zap.Uint("configID", uint(cfgRecord.ID)), zap.Error(err))
		return apperr.CheckFail(response.CheckFailCode, response.StorageNotConfigured)
	}

	// Attribute the run to a group so compute spend rolls up to whoever funds
	// it. Group-owned storage names the group directly; for personal storage we
	// fall back to the user's primary group, because the cluster time is
	// institutional either way and a null here would drop the run out of the
	// rollup entirely.
	groupID := s.attributionGroup(ctx, dto.UserId, cfgRecord)

	newJobPtr, err := createJobWithUserAndPipeline(
		s.db, dto.UserId, dto.PipelineName, dto.PipelineVersion, dto.PipelineParams, groupID, cfgRecord.ID)
	if err != nil {
		return apperr.ServerError(response.JobCreateError)
	}

	// GO-2: capture the ID, not the struct, for the detached goroutine. The
	// status writes are conditional on the row still being "submitted" so they
	// never clobber a state the monitor may set once a dispatch_id exists, and a
	// startup reconciliation pass can resolve rows left "submitted" if this pod
	// dies mid-dispatch (see ReconcileStaleDispatches).
	jobID := newJobPtr.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), nomadDispatchTimeout)
		defer cancel()
		if resp, err := s.dispatchNomadJob(ctx, dto, *storageConfig); err != nil {
			s.db.Model(&models.Job{}).Where("id = ? AND status = ?", jobID, "submitted").
				Updates(models.Job{Status: "failed"})
			log.L().Error("async nomad job dispatch failed",
				zap.Uint("userId", dto.UserId),
				zap.String("pipeline", dto.JobId()),
				zap.Error(err))
		} else {
			s.db.Model(&models.Job{}).Where("id = ? AND status = ?", jobID, "submitted").
				Updates(models.Job{Status: "dispatch_success", DispatchId: resp.DispatchedJobID})
			log.L().Info("async nomad job dispatch success",
				zap.Uint("userId", dto.UserId),
				zap.String("pipeline", dto.JobId()),
				zap.String("dispatchId", resp.DispatchedJobID))
		}
	}()

	return nil
}

// DeleteJob removes a job. Nomad cleanup is done first so that if it fails the
// DB record is preserved and the operation can be retried.
// CONS-1: Nomad deregister before DB delete to prevent orphaned Nomad jobs.
func (s *jobService) DeleteJob(dto types.JobDeleteDto) error {
	existingJob, err := GetJobById(s.db, dto.JobId)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperr.CheckFail(response.CheckFailCode, response.JobNotFound)
		}
		return apperr.ServerError(response.SystemError)
	}

	if existingJob.DispatchId != "" {
		if err := s.deleteNomadDispatchedJob(existingJob.DispatchId); err != nil {
			log.L().Error("nomad job deletion failed; aborting DB delete to allow retry",
				zap.String("dispatch_id", existingJob.DispatchId), zap.Error(err))
			return apperr.ServerError(response.JobDeletionError)
		}
	}

	resp := s.db.Delete(&existingJob)
	if resp.Error != nil || resp.RowsAffected == 0 {
		return apperr.ServerError(response.JobDeletionError)
	}

	return nil
}

// StopJob deregisters a dispatched job from Nomad and marks it "failed"
// (a user-stopped job is treated like any other failed job), keeping the DB
// record. Only jobs already running on Nomad (with a dispatch id and an active
// status) can be stopped.
func (s *jobService) StopJob(dto types.JobStopDto) error {
	existingJob, err := GetJobById(s.db, dto.JobId)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperr.CheckFail(response.CheckFailCode, response.JobNotFound)
		}
		return apperr.ServerError(response.SystemError)
	}

	if existingJob.DispatchId == "" || !isStoppableStatus(existingJob.Status) {
		return apperr.CheckFail(response.CheckFailCode, response.JobNotStoppable)
	}

	if err := s.deleteNomadDispatchedJob(existingJob.DispatchId); err != nil {
		log.L().Error("nomad job stop failed",
			zap.String("dispatch_id", existingJob.DispatchId), zap.Error(err))
		return apperr.ServerError(response.JobStopError)
	}

	// Conditional update: only flip to "failed" if the Nomad event monitor has
	// not already driven the job to a terminal state. This atomic statement
	// serializes against the monitor's FOR UPDATE transaction, so a job that
	// genuinely completed in the same instant is not clobbered back to "failed".
	// RowsAffected == 0 means it was already terminal — the user's stop intent
	// is satisfied either way, so it is not an error.
	res := s.db.Model(&models.Job{}).
		Where("id = ? AND status NOT IN ?", existingJob.ID, terminalJobStatuses).
		Update("status", "failed")
	if res.Error != nil {
		log.L().Error("failed to mark stopped job as failed",
			zap.Uint("job_id", existingJob.ID), zap.Error(res.Error))
		return apperr.ServerError(response.SystemError)
	}

	return nil
}

// GetJobDetails returns the full job record (including input params) by ID.
func (s *jobService) GetJobDetails(jobID uint) (types.JobDetailDto, error) {
	var detail types.JobDetailDto
	if err := s.db.Model(&models.Job{}).
		Select("id,pipeline_name,pipeline_version,created_at,status,dispatch_id,alloc_id,params").
		Where("id = ?", jobID).
		Scan(&detail).Error; err != nil {
		log.L().Error("get job details failed", zap.Uint("job_id", jobID), zap.Error(err))
		return detail, apperr.ServerError(response.SystemError)
	}
	if detail.ID == 0 {
		return detail, apperr.CheckFail(response.CheckFailCode, response.JobNotFound)
	}
	return detail, nil
}

// ReconcileStaleDispatches fails jobs stuck in the pre-dispatch "submitted"
// state with no dispatch_id and older than olderThan. These are jobs whose
// dispatching pod died before the async dispatch goroutine completed; without a
// dispatch_id the Nomad monitor can never resolve them. The age threshold avoids
// touching dispatches still in flight on another pod.
func (s *jobService) ReconcileStaleDispatches(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res := s.db.Model(&models.Job{}).
		Where("status = ? AND (dispatch_id IS NULL OR dispatch_id = ?) AND created_at < ?",
			"submitted", "", cutoff).
		Update("status", "failed")
	return res.RowsAffected, res.Error
}

// terminalJobStatuses are the final job states the Nomad monitor will not
// transition out of (mirrors monitor.isTerminalStatus). Used to make StopJob's
// status write conditional so it can't overwrite a genuinely-completed job.
var terminalJobStatuses = []string{"completed", "failed"}

// isStoppableStatus reports whether a job in the given status is currently
// running on Nomad and can therefore be stopped.
func isStoppableStatus(status string) bool {
	switch status {
	case "dispatch_success", "pending", "running":
		return true
	default:
		return false
	}
}

// GO-3: StreamJobLog no longer accepts *gin.Context; callers extract the writer
// and context from gin before calling.
func (s *jobService) StreamJobLog(ctx context.Context, w http.ResponseWriter, allocID, logType string) {
	opts := &StreamLogOptions{
		AllocId:  allocID,
		TaskName: s.cfg.Task,
		LogType:  logType,
	}
	StreamLogs(ctx, w, s.nomadClient, s.sse, opts)
}

func (s *jobService) dispatchNomadJob(ctx context.Context, dto types.JobAddDto, storageConfig nixstorage.MinioConfig) (*nomad.JobDispatchResponse, error) {
	var pipeline models.Pipeline
	if err := s.db.Where("name = ? AND version = ?", dto.PipelineName, dto.PipelineVersion).
		First(&pipeline).Error; err != nil {
		return nil, fmt.Errorf("pipeline not found: %w", err)
	}

	// Slurm executor + Singularity (see runner.NextflowHCL). Must not be
	// "docker": the rendered nf.config sets singularity.enabled, and Nextflow
	// refuses to run with two container engines enabled at once.
	dispatchMeta := map[string]string{"profile": "singularity"}
	jobPrefix := fmt.Sprintf("u%d", dto.UserId)

	payload := DispatchPayload{
		Repository: pipeline.Repository,
		Revision:   pipeline.Version,
		Params:     dto.PipelineParams,
		Storage: DispatchStorageConfig{
			Host:      fmt.Sprintf("%s:%d", storageConfig.Host, storageConfig.Port),
			AccessKey: storageConfig.AccessKey,
			SecretKey: storageConfig.SecretKey,
			UseSSL:    storageConfig.UseSSL,
		},
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal dispatch payload: %w", err)
	}

	resp, _, err := s.nomadJobs.Dispatch(dto.JobId(), dispatchMeta, payloadBytes, jobPrefix, (&nomad.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("dispatch nomad job failed: %w", err)
	}

	log.L().Info("nomad job dispatch successfully",
		zap.String("dispatchId", resp.DispatchedJobID),
		zap.Uint("userId", dto.UserId))

	return resp, nil
}

func (s *jobService) deleteNomadDispatchedJob(nomadDispatchID string) error {
	_, _, err := s.nomadJobs.Deregister(nomadDispatchID, false, nil)
	if err != nil {
		// Treat "not found" as success — dispatch may have already been cleaned up or never ran.
		if strings.Contains(err.Error(), "404") || strings.Contains(strings.ToLower(err.Error()), "not found") {
			log.L().Warn("nomad dispatched job not found, treating as already deleted",
				zap.String("dispatch_id", nomadDispatchID))
			return nil
		}
		return fmt.Errorf("nomad delete dispatched job failed: %w", err)
	}
	log.L().Info("nomad dispatched job deleted successfully",
		zap.String("dispatch_id", nomadDispatchID))
	return nil
}

// attributionGroup decides which group a run is billed to.
//
// Returns nil only when the user belongs to no group at all, which is the one
// case where there is genuinely nothing to attribute to.
func (s *jobService) attributionGroup(ctx context.Context, userID uint, cfg *nixstorage.StorageConfig) *uint {
	if cfg != nil && cfg.OwnerType == nixstorage.OwnerTypeGroup {
		id := cfg.OwnerID
		return &id
	}

	var membership models.GroupMembership
	err := s.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("role_in_group = 'owner' DESC, id ASC").
		First(&membership).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.L().Warn("failed to resolve attribution group",
				zap.Uint("userID", userID), zap.Error(err))
		}
		return nil
	}
	return &membership.GroupID
}

func createJobWithUserAndPipeline(
	db *gorm.DB,
	userID uint,
	pipelineName, pipelineVersion string,
	params json.RawMessage,
	groupID *uint,
	storageConfigID nixstorage.ConfigID,
) (*models.Job, error) {
	var newJob models.Job

	err := db.Transaction(func(tx *gorm.DB) error {
		var user models.User
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&user, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("user with Id %d not found", userID)
			}
			return fmt.Errorf("failed to query user: %w", err)
		}

		var pipeline models.Pipeline
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
			Where("name = ? AND version = ?", pipelineName, pipelineVersion).
			First(&pipeline).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("pipeline with name '%s' and version '%s' not found", pipelineName, pipelineVersion)
			}
			return fmt.Errorf("failed to query pipeline: %w", err)
		}

		newJob = models.Job{
			UserId:          &userID,
			PipelineId:      &pipeline.ID,
			GroupId:         groupID,
			StorageConfigId: uint(storageConfigID),
			Status:          "submitted",
			Params:          params,
			UserEmail:       user.Email,
			PipelineName:    pipeline.Name,
			PipelineVersion: pipeline.Version,
		}

		return tx.Create(&newJob).Error
	})
	if err != nil {
		return nil, err
	}
	return &newJob, nil
}

// minioConfigFor loads the credentials for an already-authorized config.
//
// Authorization happens in AddJob before this is called; this function only
// unwraps the stored secret.
func (s *jobService) minioConfigFor(configID nixstorage.ConfigID) (*nixstorage.MinioConfig, error) {
	if s.storage == nil {
		return nil, fmt.Errorf("storage manager unavailable")
	}
	cfg, err := s.storage.GetProviderConfig(configID)
	if err != nil {
		return nil, fmt.Errorf("failed to get storage config: %w", err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("storage config %d not found", configID)
	}
	var minioConfig nixstorage.MinioConfig
	if err := json.Unmarshal(cfg.RawConfig, &minioConfig); err != nil {
		return nil, fmt.Errorf("failed to parse storage config: %w", err)
	}
	return &minioConfig, nil
}

// CheckJobOwnership returns true if the given user owns the specified job.
// BUG-4: DB errors are now logged explicitly rather than silently returning false.
func CheckJobOwnership(db *gorm.DB, userID, jobID uint) bool {
	var job models.Job
	if err := db.Where("id = ?", jobID).First(&job).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.L().Error("check job ownership db error",
				zap.Uint("user_id", userID), zap.Uint("job_id", jobID), zap.Error(err))
		}
		return false
	}
	if job.UserId == nil {
		return false
	}
	return userID == *job.UserId
}

// CheckAllocOwnership returns true if the given user owns the job for the allocation.
// BUG-4: DB errors are now logged explicitly rather than silently returning false.
func CheckAllocOwnership(db *gorm.DB, userID uint, allocID string) bool {
	var job models.Job
	if err := db.Where("alloc_id = ?", allocID).First(&job).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.L().Error("check alloc ownership db error",
				zap.Uint("user_id", userID), zap.String("alloc_id", allocID), zap.Error(err))
		}
		return false
	}
	if job.UserId == nil {
		return false
	}
	return userID == *job.UserId
}

// GetJobById returns a job record by ID.
// BUG-4: errors from db.First are now propagated instead of checked via ID == 0.
func GetJobById(db *gorm.DB, jobID uint) (models.Job, error) {
	var job models.Job
	if err := db.Where("id = ?", jobID).First(&job).Error; err != nil {
		return job, fmt.Errorf("query job %d: %w", jobID, err)
	}
	return job, nil
}

// DispatchPayload is sent to Nomad dispatch.
type DispatchPayload struct {
	Repository string                `json:"repository"`
	Revision   string                `json:"revision"`
	Params     json.RawMessage       `json:"params"`
	Storage    DispatchStorageConfig `json:"storage"`
}

// DispatchStorageConfig holds S3/MinIO credentials for the job.
type DispatchStorageConfig struct {
	Host      string `json:"host"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	UseSSL    bool   `json:"use_ssl"`
}
