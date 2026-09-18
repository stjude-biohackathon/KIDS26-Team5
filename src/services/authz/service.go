package authz

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"antelope/internal/modules/log"
	nixstorage "antelope/internal/modules/storage"
	"antelope/models"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// casbinModel is the PERM definition.
//
//	sub — "user:42" or "group:7"
//	obj — "storage:15"
//	act — read | write | admin
//
// g relates a user to the groups they belong to, so a grant written against a
// group is reachable by every member without duplicating rows per user.
//
// Deliberately no level-implication logic in the matcher: a write grant is
// expanded into explicit read and write rows when the policy is built. That
// keeps a denial explainable by pointing at a row that is or is not present,
// rather than at matcher arithmetic nobody can read.
const casbinModel = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = g(r.sub, p.sub) && r.obj == p.obj && r.act == p.act
`

const (
	// policyEpochKey is bumped whenever the domain tables change so other
	// replicas notice and rebuild their in-memory policy.
	policyEpochKey = "authz:policy:epoch"

	// freshnessWindow bounds how often a replica asks Redis whether the policy
	// changed. Within the window a request uses the in-memory policy directly.
	// This is the staleness ceiling for a revocation made on another replica.
	freshnessWindow = 2 * time.Second
)

// service is the Casbin-backed Authorizer.
//
// Policy is held in memory and rebuilt from the domain tables — there is no
// casbin_rule table and no persistence adapter. The domain tables (groups,
// group_memberships, storage_grants, storage_configs) are the only source of
// truth, which removes the dual-write hazard entirely: policy cannot drift
// from the data it is derived from because it holds nothing of its own.
//
// This mirrors the Postgres-durable / cache-rebuildable pattern already used
// for storage and LLM configs elsewhere in the codebase.
type service struct {
	db    *gorm.DB
	redis redis.UniversalClient

	mu       sync.RWMutex
	enforcer *casbin.Enforcer

	freshMu   sync.Mutex
	lastEpoch int64
	lastCheck time.Time
}

// New builds an Authorizer and performs the initial policy load.
//
// redisClient may be nil in single-process or test setups; the policy is then
// only rebuilt by in-process mutations, which is correct for one replica.
func New(db *gorm.DB, redisClient redis.UniversalClient) (Authorizer, error) {
	s := &service{db: db, redis: redisClient}

	if err := s.ReloadPolicy(context.Background()); err != nil {
		return nil, fmt.Errorf("authz: initial policy load: %w", err)
	}
	return s, nil
}

// ── Policy construction ───────────────────────────────────────────────────────

// storageOwnerRow is a narrow projection of storage_configs. The table is owned
// by internal/modules/storage, which deliberately does not import the domain
// models package; reading the two columns policy needs by name avoids inverting
// that dependency for the sake of a projection.
type storageOwnerRow struct {
	ID      uint
	OwnerID uint
}

// ReloadPolicy rebuilds the enforcer from the domain tables and swaps it in.
func (s *service) ReloadPolicy(ctx context.Context) error {
	m, err := model.NewModelFromString(casbinModel)
	if err != nil {
		return fmt.Errorf("authz: parse model: %w", err)
	}

	e, err := casbin.NewEnforcer(m)
	if err != nil {
		return fmt.Errorf("authz: create enforcer: %w", err)
	}
	// Policy lives in memory only; nothing should try to persist it.
	e.EnableAutoSave(false)

	var memberships []models.GroupMembership
	if err := s.db.WithContext(ctx).Find(&memberships).Error; err != nil {
		return fmt.Errorf("authz: load memberships: %w", err)
	}
	for _, mem := range memberships {
		if _, err := e.AddGroupingPolicy(SubjectUser(mem.UserID), SubjectGroup(mem.GroupID)); err != nil {
			return fmt.Errorf("authz: add membership rule: %w", err)
		}
	}

	var grants []models.StorageGrant
	if err := s.db.WithContext(ctx).Find(&grants).Error; err != nil {
		return fmt.Errorf("authz: load storage grants: %w", err)
	}
	for _, g := range grants {
		obj := StorageResource(g.StorageConfigID).String()
		for _, act := range models.ImpliedAccess(g.AccessLevel) {
			if _, err := e.AddPolicy(SubjectGroup(g.GroupID), obj, act); err != nil {
				return fmt.Errorf("authz: add grant rule: %w", err)
			}
		}
	}

	// Personal configs become an implicit self-grant at admin level. Modelling
	// them as policy rather than a special case in Can keeps exactly one code
	// path through enforcement — the rarely-exercised branch is where a bug
	// would otherwise hide.
	var personal []storageOwnerRow
	err = s.db.WithContext(ctx).
		Table("storage_configs").
		Select("id", "owner_id").
		Where("owner_type = ? AND deleted_at IS NULL", nixstorage.OwnerTypePersonal).
		Scan(&personal).Error
	switch {
	case err == nil:
		for _, p := range personal {
			obj := StorageResource(p.ID).String()
			for _, act := range models.ImpliedAccess(models.AccessAdmin) {
				if _, addErr := e.AddPolicy(SubjectUser(p.OwnerID), obj, act); addErr != nil {
					return fmt.Errorf("authz: add personal rule: %w", addErr)
				}
			}
		}
	case isMissingTable(err):
		// First boot ordering: the storage module migrates its own table and
		// may not have run yet. Group grants still load; personal rules arrive
		// on the next reload.
		log.L().Warn("authz: storage_configs not present yet, skipping personal rules")
	default:
		return fmt.Errorf("authz: load personal configs: %w", err)
	}

	s.mu.Lock()
	s.enforcer = e
	s.mu.Unlock()

	s.freshMu.Lock()
	s.lastEpoch = s.readEpoch(ctx)
	s.lastCheck = time.Now()
	s.freshMu.Unlock()

	log.L().Debug("authz: policy reloaded",
		zap.Int("memberships", len(memberships)),
		zap.Int("grants", len(grants)),
		zap.Int("personalConfigs", len(personal)))
	return nil
}

// isMissingTable reports whether err is Postgres "relation does not exist".
func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "does not exist") || strings.Contains(msg, "no such table")
}

// ── Freshness ─────────────────────────────────────────────────────────────────

func (s *service) readEpoch(ctx context.Context) int64 {
	if s.redis == nil {
		return 0
	}
	raw, err := s.redis.Get(ctx, policyEpochKey).Result()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	if err != nil {
		// Fail-open on freshness only: serve the in-memory policy rather than
		// failing every authorization check because Redis is unreachable.
		return s.lastEpoch
	}
	n, convErr := strconv.ParseInt(raw, 10, 64)
	if convErr != nil {
		return s.lastEpoch
	}
	return n
}

// BumpEpoch signals other replicas that the policy changed. Called by the
// grant/membership mutation paths after their transaction commits.
func (s *service) BumpEpoch(ctx context.Context) {
	if s.redis == nil {
		return
	}
	if err := s.redis.Incr(ctx, policyEpochKey).Err(); err != nil {
		log.L().Warn("authz: failed to bump policy epoch; other replicas may lag",
			zap.Error(err))
	}
}

// ensureFresh rebuilds the policy when another replica has changed it. Checks
// are rate-limited to freshnessWindow so a hot path does not hit Redis per call.
func (s *service) ensureFresh(ctx context.Context) {
	if s.redis == nil {
		return
	}

	s.freshMu.Lock()
	if time.Since(s.lastCheck) < freshnessWindow {
		s.freshMu.Unlock()
		return
	}
	s.lastCheck = time.Now()
	known := s.lastEpoch
	s.freshMu.Unlock()

	current := s.readEpoch(ctx)
	if current == known {
		return
	}

	if err := s.ReloadPolicy(ctx); err != nil {
		log.L().Error("authz: policy reload failed; serving previous policy", zap.Error(err))
	}
}

// ── Enforcement ───────────────────────────────────────────────────────────────

func (s *service) Can(ctx context.Context, userID uint, action Action, res Resource) (bool, error) {
	if userID == 0 {
		return false, nil
	}

	if s.isSuper(ctx, userID) {
		// Platform admins bypass resource grants by design, but never silently:
		// an unaudited bypass is indistinguishable from a policy hole.
		log.L().Info("authz: super-user bypass",
			zap.Uint("userID", userID),
			zap.String("action", string(action)),
			zap.String("resource", res.String()))
		return true, nil
	}

	s.ensureFresh(ctx)

	s.mu.RLock()
	e := s.enforcer
	s.mu.RUnlock()

	ok, err := e.Enforce(SubjectUser(userID), res.String(), string(action))
	if err != nil {
		return false, fmt.Errorf("authz: enforce: %w", err)
	}
	return ok, nil
}

func (s *service) Explain(ctx context.Context, userID uint, action Action, res Resource) (Decision, error) {
	if s.isSuper(ctx, userID) {
		log.L().Info("authz: super-user bypass",
			zap.Uint("userID", userID),
			zap.String("action", string(action)),
			zap.String("resource", res.String()))
		return Decision{Allowed: true, SuperBypass: true}, nil
	}

	s.ensureFresh(ctx)

	s.mu.RLock()
	e := s.enforcer
	s.mu.RUnlock()

	ok, matched, err := e.EnforceEx(SubjectUser(userID), res.String(), string(action))
	if err != nil {
		return Decision{}, fmt.Errorf("authz: enforce: %w", err)
	}

	if ok {
		d := Decision{Allowed: true}
		// matched is the policy row that allowed it: [sub, obj, act].
		if len(matched) > 0 {
			if ref := s.groupRefFromSubject(ctx, matched[0]); ref != nil {
				d.GrantedVia = ref
			}
		}
		return d, nil
	}

	d := Decision{Allowed: false, Reason: DenyNoGrant}

	// Distinguish "cannot reach it at all" from "can read but not write" —
	// they lead the user to different next steps.
	if action != ActionRead {
		if readOK, readErr := e.Enforce(SubjectUser(userID), res.String(), string(ActionRead)); readErr == nil && readOK {
			d.Reason = DenyInsufficientLevel
		}
	}

	d.RequestAccessFrom = s.groupsHolding(ctx, res)
	return d, nil
}

func (s *service) AccessibleStorageIDs(ctx context.Context, userID uint, action Action) ([]nixstorage.ConfigID, error) {
	if userID == 0 {
		return nil, nil
	}

	if s.isSuper(ctx, userID) {
		var ids []nixstorage.ConfigID
		err := s.db.WithContext(ctx).
			Table("storage_configs").
			Where("deleted_at IS NULL").
			Pluck("id", &ids).Error
		if err != nil && !isMissingTable(err) {
			return nil, fmt.Errorf("authz: list storage configs: %w", err)
		}
		return ids, nil
	}

	s.ensureFresh(ctx)

	s.mu.RLock()
	e := s.enforcer
	s.mu.RUnlock()

	// Ask the engine for every object this subject reaches, rather than
	// enumerating configs and calling Enforce per row.
	perms, err := e.GetImplicitPermissionsForUser(SubjectUser(userID))
	if err != nil {
		return nil, fmt.Errorf("authz: implicit permissions: %w", err)
	}

	seen := make(map[nixstorage.ConfigID]struct{})
	ids := make([]nixstorage.ConfigID, 0, len(perms))
	for _, p := range perms {
		if len(p) < 3 || p[2] != string(action) {
			continue
		}
		id, ok := parseStorageObject(p[1])
		if !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// parseStorageObject extracts the numeric ID from a "storage:15" token.
func parseStorageObject(obj string) (nixstorage.ConfigID, bool) {
	prefix := string(KindStorage) + ":"
	if len(obj) <= len(prefix) || obj[:len(prefix)] != prefix {
		return 0, false
	}
	n, err := strconv.ParseUint(obj[len(prefix):], 10, 64)
	if err != nil {
		return 0, false
	}
	return nixstorage.ConfigID(n), true
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// isSuper reports whether the user holds the platform super role. Read from the
// database rather than the JWT so a demotion takes effect immediately.
func (s *service) isSuper(ctx context.Context, userID uint) bool {
	if userID == 0 {
		return false
	}
	var role string
	err := s.db.WithContext(ctx).
		Model(&models.User{}).
		Where("id = ?", userID).
		Pluck("role", &role).Error
	if err != nil {
		log.L().Warn("authz: failed to read user role; treating as non-super",
			zap.Uint("userID", userID), zap.Error(err))
		return false
	}
	return role == "super"
}

// groupRefFromSubject turns a "group:7" policy subject into a display ref.
func (s *service) groupRefFromSubject(ctx context.Context, subject string) *GroupRef {
	prefix := "group:"
	if len(subject) <= len(prefix) || subject[:len(prefix)] != prefix {
		return nil
	}
	id, err := strconv.ParseUint(subject[len(prefix):], 10, 64)
	if err != nil {
		return nil
	}

	var g models.Group
	if err := s.db.WithContext(ctx).First(&g, uint(id)).Error; err != nil {
		return nil
	}
	return &GroupRef{ID: g.ID, Name: g.Name, DisplayName: g.DisplayName}
}

// groupsHolding lists the groups already granted res, with their owners, so a
// denial can name someone who is able to grant access.
func (s *service) groupsHolding(ctx context.Context, res Resource) []GroupRef {
	if res.Kind != KindStorage {
		return nil
	}

	var grants []models.StorageGrant
	if err := s.db.WithContext(ctx).
		Where("storage_config_id = ?", res.ID).
		Find(&grants).Error; err != nil {
		return nil
	}

	refs := make([]GroupRef, 0, len(grants))
	for _, g := range grants {
		var group models.Group
		if err := s.db.WithContext(ctx).First(&group, g.GroupID).Error; err != nil {
			continue
		}

		var emails []string
		if err := s.db.WithContext(ctx).
			Model(&models.User{}).
			Joins("JOIN group_memberships gm ON gm.user_id = users.id AND gm.deleted_at IS NULL").
			Where("gm.group_id = ? AND gm.role_in_group = ?", group.ID, models.GroupRoleOwner).
			Pluck("users.email", &emails).Error; err != nil {
			log.L().Debug("authz: failed to resolve group owners",
				zap.Uint("groupID", group.ID), zap.Error(err))
		}

		refs = append(refs, GroupRef{
			ID:          group.ID,
			Name:        group.Name,
			DisplayName: group.DisplayName,
			OwnerEmails: emails,
		})
	}
	return refs
}
