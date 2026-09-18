// Package routers owns HTTP route registration.
//
// RouterManager is the single entry-point: it receives an AppDeps (the narrow
// interface over *app.App), builds all services and handlers from the injected
// clients, and registers every route group onto a *gin.Engine.
//
// Nothing in this package imports *app.App; it only depends on AppDeps.
// This makes the package independently testable: pass a stub AppDeps in tests.
package routers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"antelope/internal/modules/agent"
	"antelope/internal/modules/auth/session"
	"antelope/internal/modules/email"
	llmcfgmod "antelope/internal/modules/llmconfig"
	"antelope/internal/modules/log"
	"antelope/internal/modules/nosql"
	"antelope/internal/modules/runner"
	"antelope/internal/modules/setting"
	"antelope/internal/modules/sse"
	"antelope/internal/modules/storage"
	"antelope/internal/modules/webui"
	"antelope/pkg/secretbox"
	v1 "antelope/routers/api/v1"
	"antelope/routers/middleware"
	agentsvc "antelope/services/agent"
	agenttools "antelope/services/agent/tools"
	apikeysvc "antelope/services/apikey"
	auditsvc "antelope/services/audit"
	authsvc "antelope/services/auth"
	authzsvc "antelope/services/authz"
	cachesvc "antelope/services/cache"
	dashsvc "antelope/services/dashboard"
	groupsvc "antelope/services/group"
	jobsvc "antelope/services/job"
	llmcfgsvc "antelope/services/llmconfig"
	notifsvc "antelope/services/notification"
	pipelinesvc "antelope/services/pipeline"
	osssvc "antelope/services/storage"
	usersvc "antelope/services/user"

	_ "antelope/api" // registers the generated Swagger docs (swaggo) for /api/docs/

	"github.com/gin-gonic/gin"
	nomad "github.com/hashicorp/nomad/api"
	"github.com/redis/go-redis/v9"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// AppDeps is the narrow interface RouterManager depends on.
// *app.App satisfies it; tests can supply a lightweight stub.
// Duplicated here (rather than imported from the app package) so that the
// routers package has no import dependency on app.
type AppDeps interface {
	GetConfig() setting.ServerConfig
	GetDB() *gorm.DB
	GetRedis() redis.UniversalClient
	GetNomad() *nomad.Client
	GetStorage() *storage.ClientManager
	GetMailer() *email.Mailer
	GetSSE() *sse.Manager
	GetLLMConfig() *llmcfgmod.Manager
	GetAgent() *agent.Manager
	GetSecretBox() *secretbox.Box
}

// RouterManager builds all services and registers all routes.
// Construct it with New, then call Engine() or MonitorEngine().
type RouterManager struct {
	deps         AppDeps
	cfg          setting.ServerConfig
	sessionStore session.Store

	// pre-built services (set in buildServices)
	authSvc   authsvc.Service
	userSvc   usersvc.Service
	pipeSvc   pipelinesvc.Service
	jobSvc    jobsvc.Service
	ossSvc    osssvc.Service
	dashSvc   dashsvc.Service
	cacheSvc  cachesvc.Service
	agentSvc  agentsvc.Service
	llmCfgSvc llmcfgsvc.Service
	notifSvc  notifsvc.Service
	apikeySvc apikeysvc.Service
	authzSvc  authzsvc.Authorizer
	groupSvc  groupsvc.Service
	auditSvc  auditsvc.Recorder
}

// New creates a RouterManager and eagerly builds all services.
// Panics on service initialisation failure (e.g. invalid LLM config) so the
// problem surfaces at startup rather than silently at request time.
func New(deps AppDeps) *RouterManager {
	rm := &RouterManager{
		deps: deps,
		cfg:  deps.GetConfig(),
	}
	if err := rm.buildServices(); err != nil {
		panic(fmt.Sprintf("routers: service init failed: %v", err))
	}
	return rm
}

// Engine returns a fully-wired *gin.Engine for the main API server.
func (rm *RouterManager) Engine() *gin.Engine {
	gin.SetMode(rm.cfg.System.GetGinMode())

	r := gin.New()
	r.Use(gin.Recovery())
	// Per-request logging (our structured request logger + gin's own access log)
	// is only useful in debug mode. In release it emits a line for every request,
	// which floods the logs with low-value noise, so we skip it entirely.
	if gin.Mode() == gin.DebugMode {
		r.Use(middleware.RequestLogger())
		r.Use(gin.Logger())
	}
	r.Use(middleware.CORSMiddleware(rm.cfg.Cors))

	rm.registerPublicAndPrivateRoutes(r)

	// Swagger UI
	r.GET("/api/docs/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	// Embedded frontend (only when built with `-tags embed`); serves the SPA
	// for any route not claimed by the API above.
	if webui.Enabled() {
		r.NoRoute(webui.Handler())
	}

	return r
}

// MonitorEngine returns a minimal *gin.Engine for the standalone monitor process.
func (rm *RouterManager) MonitorEngine() *gin.Engine {
	gin.SetMode(rm.cfg.System.GetGinMode())

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(middleware.CORSMiddleware(rm.cfg.Cors))

	pub := r.Group("/api/v1")
	probeH := v1.NewProbeHandler(rm.deps.GetDB(), rm.deps.GetRedis(), rm.deps.GetNomad())
	pub.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, "ok") })
	pub.GET("/ready", probeH.Ready)

	return r
}

// ── service construction ───────────────────────────────────────────────────

func (rm *RouterManager) buildServices() error { //nolint:unparam // error return reserved for service-init failures; keeps New()'s error handling stable
	db := rm.deps.GetDB()
	rdb := rm.deps.GetRedis()
	nomadC := rm.deps.GetNomad()
	stor := rm.deps.GetStorage()
	mailer := rm.deps.GetMailer()
	sseM := rm.deps.GetSSE()
	// The Nomad task name is an internal constant shared with the HCL
	// templates (the log-streamer must address the task by this exact name).
	taskCfg := runner.TaskName
	jwtCfg := rm.cfg.Jwt

	nomadJobs := nomadC.Jobs()

	rm.sessionStore = session.NewRedisStore(rdb)

	rm.authSvc = authsvc.NewService(db, jwtCfg, rm.cfg.System.IsProduction(), rm.sessionStore, rdb, rm.deps.GetSecretBox())
	rm.userSvc = usersvc.NewService(db, rm.sessionStore)
	rm.pipeSvc = pipelinesvc.NewService(db, rdb, nomadJobs, rm.cfg.Nomad)
	// Authorization is built before the services that consult it. A failure
	// here is fatal by design: starting with an empty policy would silently
	// deny every user rather than fail loudly.
	authorizer, err := authzsvc.New(db, rdb)
	if err != nil {
		return fmt.Errorf("build authorizer: %w", err)
	}
	rm.authzSvc = authorizer

	rm.auditSvc = auditsvc.NewRecorder(db)
	rm.ossSvc = osssvc.NewOssService(stor, authorizer, rm.auditSvc, db, rm.cfg.System.PersonalStorageAllowed())
	rm.groupSvc = groupsvc.NewService(db, stor, authorizer)
	rm.jobSvc = jobsvc.NewService(db, nomadJobs, nomadC, jobsvc.Config{Task: taskCfg}, sseM, stor, rm.ossSvc)
	rm.dashSvc = dashsvc.NewService(db)
	rm.cacheSvc = cachesvc.NewService(db, rdb, mailer)

	llmCfgMgr := rm.deps.GetLLMConfig()
	rm.llmCfgSvc = llmcfgsvc.NewService(llmCfgMgr)
	rm.notifSvc = notifsvc.NewService(db)
	rm.apikeySvc = apikeysvc.NewService(db, rm.authSvc, rm.sessionStore)

	// SEC: configure the external CAB upstream from config (cab.base-url) for
	// both the HTTP proxy handlers and the agent tools, instead of a baked-in host.
	v1.SetCABBaseURL(rm.cfg.Cab.BaseURL)

	agentMgr := rm.deps.GetAgent()
	agentMgr.SetCustomTools(agenttools.NewBuiltinTools(agenttools.Deps{
		DB:         db,
		Storage:    stor,
		Jobs:       rm.jobSvc,
		Schemas:    rm.pipeSvc,
		Dash:       rm.dashSvc,
		Notifs:     rm.notifSvc,
		CabBaseURL: rm.cfg.Cab.BaseURL,
	}))
	rm.agentSvc = agentsvc.NewService(agentsvc.Deps{
		Agent:   agentMgr,
		DB:      db,
		Storage: stor,
		SSE:     sseM,
		Cfg:     rm.cfg.Agent,
	})
	return nil
}

// reconcileStaleRecords resolves records left in a transient state ("submitted"
// jobs / "pending" pipelines) by a pod that died mid-dispatch or mid-registration.
// It runs under a Redis lock (nosql.RunOnce) so that, across a multi-pod fleet,
// only one instance performs the sweep; waitTimeout bounds how long losers wait
// before skipping.
func (rm *RouterManager) reconcileStaleRecords(ctx context.Context, waitTimeout time.Duration) {
	const staleAfter = 10 * time.Minute // well beyond a normal dispatch window

	_ = nosql.RunOnce(ctx, rm.deps.GetRedis(), nosql.LockOptions{
		Key:         "antelope:reconcile-stale",
		TTL:         30 * time.Second,
		WaitTimeout: waitTimeout,
		Poll:        time.Second,
	}, func() error {
		if n, err := rm.jobSvc.ReconcileStaleDispatches(staleAfter); err != nil {
			log.L().Warn("reconcile stale job dispatches failed", zap.Error(err))
		} else if n > 0 {
			log.L().Info("reconciled stale job dispatches", zap.Int64("count", n))
		}
		if n, err := rm.pipeSvc.ReconcileStalePending(staleAfter); err != nil {
			log.L().Warn("reconcile stale pipeline registrations failed", zap.Error(err))
		} else if n > 0 {
			log.L().Info("reconciled stale pipeline registrations", zap.Int64("count", n))
		}
		return nil
	}, nil) // verify=nil: losers skip; the winner's sweep covers all rows
}

// RunStartupReconciliation performs a one-shot reconciliation sweep at boot.
func (rm *RouterManager) RunStartupReconciliation() {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	rm.reconcileStaleRecords(ctx, 30*time.Second)
}

// RunPeriodicReconciliation keeps sweeping stale transient records on an interval
// until ctx is cancelled. The startup pass alone is insufficient on a long-lived
// fleet: a pod that crashes mid-dispatch would otherwise leave jobs stuck in
// "submitted" until some pod restarts. Redis-locked, so only one pod sweeps per
// tick; losers use a short wait and skip promptly.
func (rm *RouterManager) RunPeriodicReconciliation(ctx context.Context) {
	const interval = 5 * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rm.reconcileStaleRecords(ctx, 5*time.Second)
		}
	}
}

// ── route registration ─────────────────────────────────────────────────────

func (rm *RouterManager) registerPublicAndPrivateRoutes(r *gin.Engine) {
	// ── Middleware factories ──────────────────────────────────────────────
	// ARCH-1: role middleware no longer needs *gorm.DB; role is read from JWT claims.
	authMW := middleware.NewAuthMiddleware(rm.cfg.Jwt, rm.sessionStore)
	adminOrSuperMW := middleware.NewRoleMiddleware(middleware.RoleAdmin, middleware.RoleSuper)
	superOnlyMW := middleware.NewRoleMiddleware(middleware.RoleSuper)
	setRoleMW := middleware.NewSetRoleMiddleware()

	// ── Handlers ─────────────────────────────────────────────────────────
	db := rm.deps.GetDB()
	authH := v1.NewAuthHandler(rm.authSvc)
	userH := v1.NewUserHandler(rm.userSvc, rm.cacheSvc)
	pipeH := v1.NewPipelineHandler(rm.pipeSvc)
	jobH := v1.NewJobHandler(rm.jobSvc)
	ossH := v1.NewOssHandler(rm.ossSvc)
	dashH := v1.NewDashboardHandler(rm.dashSvc)
	agentH := v1.NewAgentHandler(rm.agentSvc)
	llmCfgH := v1.NewLLMConfigHandler(rm.llmCfgSvc)
	probeH := v1.NewProbeHandler(rm.deps.GetDB(), rm.deps.GetRedis(), rm.deps.GetNomad())
	tplH := v1.NewJobTemplateHandler(db)
	notifH := v1.NewNotificationHandler(rm.notifSvc, rm.deps.GetRedis(), rm.deps.GetSSE())
	apikeyH := v1.NewAPIKeyHandler(rm.apikeySvc)
	groupH := v1.NewGroupHandler(rm.groupSvc)
	auditH := v1.NewAuditHandler(rm.auditSvc)

	// ── Route groups ─────────────────────────────────────────────────────
	pub := r.Group("/api/v1")
	priv := r.Group("/api/v1")
	priv.Use(authMW)

	rm.registerHealthRoutes(pub, probeH)
	rm.registerAuthRoutes(pub, priv, authH)
	rm.registerUserRoutes(pub, priv, userH, jobH, dashH, setRoleMW)
	rm.registerJobRoutes(priv, jobH)
	rm.registerPipelineRoutes(pub, priv, pipeH, adminOrSuperMW)
	rm.registerTemplateRoutes(pub, priv, tplH, adminOrSuperMW)
	rm.registerStorageRoutes(priv, ossH)
	rm.registerAdminRoutes(priv, dashH, adminOrSuperMW)
	rm.registerSystemRoutes(priv, userH, authH, superOnlyMW)
	rm.registerAgentRoutes(priv, agentH)
	rm.registerLLMConfigRoutes(priv, llmCfgH)
	rm.registerProxyRoutes(priv)
	rm.registerNotificationRoutes(priv, notifH)
	rm.registerAPIKeyRoutes(priv, apikeyH)
	rm.registerGroupRoutes(priv, groupH, superOnlyMW)
	rm.registerAuditRoutes(priv, auditH, superOnlyMW)
}
