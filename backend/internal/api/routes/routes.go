package routes

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"backend/internal/api/handlers"
	"backend/internal/api/middleware"
	"backend/internal/auth"
	"backend/internal/config"
	"backend/internal/health"
	"backend/internal/models"
	"backend/internal/sessionstore"
	"backend/internal/websocket"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
)

// ErrAuthDepsMissing is returned by Setup when AUTH_ENABLED is true and an
// auth dependency is missing. The authenticated router is never built without
// its session store: every revocation check would be skipped.
var ErrAuthDepsMissing = errors.New("auth is enabled but an auth dependency is missing")

// RateLimiters groups the rate limiters created by Setup. The auth limiters
// are nil with AUTH_ENABLED=false.
type RateLimiters struct {
	// API is RATE_LIMIT per client IP on every /api/v1 route (and on the /ws
	// upgrade with auth on).
	API *handlers.RateLimiter
	// Login is LOGIN_RATE_LIMIT per client IP on POST /auth/login.
	Login *handlers.RateLimiter
	// Register is LOGIN_RATE_LIMIT per client IP on POST /auth/register.
	Register *handlers.RateLimiter
	// OIDCLogin is LOGIN_RATE_LIMIT per client IP on GET /auth/oidc/login (it
	// writes one session store row per call).
	OIDCLogin *handlers.RateLimiter
	// PasswordChange is LOGIN_RATE_LIMIT per client IP on PUT /auth/me/password
	// (each request costs up to two bcrypt operations). It has its own budget:
	// it never uses up the login budget of a shared address.
	PasswordChange *handlers.RateLimiter
	// OIDCCallback is LOGIN_RATE_LIMIT per client IP on GET /auth/oidc/callback
	// (it calls the identity provider). It has its own budget: a user who
	// starts a login must still have the budget for the return.
	OIDCCallback *handlers.RateLimiter
}

// Stop terminates the background cleanup goroutines. It is idempotent.
func (r *RateLimiters) Stop() {
	if r == nil {
		return
	}
	for _, l := range []*handlers.RateLimiter{r.API, r.Login, r.Register, r.OIDCLogin, r.OIDCCallback, r.PasswordChange} {
		if l != nil {
			l.Stop()
		}
	}
}

// AuthDeps are the dependencies of the auth module. Setup needs all of them,
// except OIDCProvider, when AUTH_ENABLED is true.
type AuthDeps struct {
	Users         models.UserRepository
	RefreshTokens models.RefreshTokenRepository
	APIKeys       models.APIKeyRepository
	AuditLogs     models.AuditLogRepository
	SessionStore  sessionstore.SessionStore
	// OIDCProvider is the identity provider client. Nil: Setup builds it from
	// Config.OIDC when OIDC_ENABLED is true (no network at construction).
	OIDCProvider handlers.OIDCProvider
	// RevalidateInterval is the time between two revocation checks of the
	// open sockets; 0 uses websocket.DefaultRevalidateInterval.
	RevalidateInterval time.Duration
}

// Deps are the dependencies of Setup.
type Deps struct {
	Config        *config.Config
	Repository    models.Repository
	HealthChecker *health.HealthChecker
	Hub           *websocket.Hub
	// Auth is required when Config.Auth.Enabled and ignored otherwise.
	Auth *AuthDeps
}

// Result is what Setup built and what the caller must stop.
type Result struct {
	// RateLimiters must be stopped at shutdown.
	RateLimiters *RateLimiters
	// AuthHandler runs EnsureAdminUser and CleanupExpiredTokens. Nil with
	// auth off.
	AuthHandler *handlers.AuthHandler
	// AuditRecorder must be shut down before the database closes. Nil with
	// auth off.
	AuditRecorder *middleware.AuditRecorder
}

// isNil reports whether v is nil or a nil pointer in an interface.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// checkDeps validates deps before anything is registered or started.
func checkDeps(deps Deps) error {
	if deps.Config == nil {
		return errors.New("routes: no configuration")
	}
	if !deps.Config.Auth.Enabled {
		return nil
	}
	a := deps.Auth
	if a == nil {
		return fmt.Errorf("%w: no auth dependencies", ErrAuthDepsMissing)
	}
	for name, v := range map[string]any{
		"session store": a.SessionStore, "user repository": a.Users, "refresh token repository": a.RefreshTokens,
		"API key repository": a.APIKeys, "audit log repository": a.AuditLogs,
	} {
		if isNil(v) {
			return fmt.Errorf("%w: %s", ErrAuthDepsMissing, name)
		}
	}
	return nil
}

// Setup registers every route on router. It is the production route builder:
// the bootstrap and the tests use it.
//
// With AUTH_ENABLED=false no auth middleware is registered: Items, /ws and
// ping are open whatever credentials a request carries, and the auth, API
// key, user and audit log routes do not exist (404).
//
// With AUTH_ENABLED=true:
//   - Public: /health/*, GET /api/v1/ui-config, POST /auth/login (LOGIN_RATE_LIMIT,
//     same-site check when it sets the cookie), POST /auth/register
//     (LOGIN_RATE_LIMIT, its own budget), POST /auth/refresh (no dedicated
//     limiter: RATE_LIMIT only), POST /auth/logout (the handler
//     authenticates itself from the bearer token and the refresh cookie),
//     GET /auth/oidc/login and /callback (LOGIN_RATE_LIMIT each, one budget
//     per route, no same-site check: they are top-level navigations).
//   - Every other /api/v1 route: CombinedAuth (JWT or API key, revocation
//     checks fail closed on a store error), then the audit middleware.
//     /auth/logout-all, PUT /auth/me/password (its own LOGIN_RATE_LIMIT
//     budget, audited although it is under /auth/) and /api-keys also need a
//     login session (RequireSessionAuth); /users and /audit-logs need the
//     admin role.
//   - /ws: RATE_LIMIT, a token (header, or ?token= for browsers) or an API
//     key, an active user, a re-check after the registration, a periodic
//     revalidation of the open sockets (fail closed), and authorised topic
//     subscriptions.
//
// Setup returns an error, and registers nothing, when the configuration is
// missing or AUTH_ENABLED is true without all auth dependencies
// (ErrAuthDepsMissing) or with an OIDC configuration that cannot be used.
//
// Precondition: Config.Server.RateLimit > 0 and Config.Server.LoginRateLimit
// > 0. config.LoadConfig guarantees both (it rejects a value <= 0).
func Setup(router *gin.Engine, deps Deps) (*Result, error) {
	if err := checkDeps(deps); err != nil {
		return nil, err
	}
	cfg := deps.Config

	var a *authWiring
	if cfg.Auth.Enabled {
		var err error
		if a, err = newAuthWiring(cfg, deps.Auth, deps.Hub); err != nil {
			return nil, err
		}
	}

	setTrustedProxies(router, cfg.Server.TrustedProxies)

	// No redirects of any kind. Gin answers a path with or without a trailing
	// slash (and, with RedirectFixedPath, a path in another case) with a 301
	// before any middleware runs, and in debug mode prints the target, query
	// string included, to its own writer: the /ws token would reach the log
	// ahead of RedactWSToken. A request that does not match a route is a 404.
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false

	// Add middleware. RedactWSToken runs before Logger and the telemetry
	// middleware so the WebSocket token in the query string is never recorded
	// in a log line, a span attribute or a metric label.
	router.Use(middleware.RequestID())
	router.Use(middleware.RedactWSToken())
	// Telemetry: the providers are installed by telemetry.Init before Setup.
	// With both switches off nothing is registered and the auth counters are
	// bound to the no-op provider, so a request does no extra work.
	middleware.RebindAuthMeter()
	if cfg.Otel.Enabled || cfg.Otel.MetricsEnabled {
		router.Use(middleware.HTTPMetrics())
	}
	if cfg.Otel.Enabled {
		// otelgin also records http.server.request.duration and body-size
		// histograms through the global provider. HTTPMetrics is the one
		// recorder of request metrics (its own buckets and bounded labels),
		// so otelgin gets a no-op meter and only traces.
		router.Use(otelgin.Middleware(cfg.Otel.ServiceName, otelgin.WithMeterProvider(noopmetric.NewMeterProvider())))
	}
	router.Use(middleware.Logger())
	router.Use(middleware.Recovery())
	router.Use(middleware.SecurityHeaders())
	router.Use(middleware.CORS(cfg.CORS.AllowedOrigins))
	router.Use(middleware.MaxBodySize(1 << 20)) // 1 MB default

	// Rate limiter for API routes: RATE_LIMIT requests per minute per client IP.
	limiters := &RateLimiters{
		API:   handlers.NewRateLimiter(int(cfg.Server.RateLimit), time.Minute),
		Login: handlers.NewRateLimiter(int(cfg.Server.LoginRateLimit), time.Minute),
	}

	wsHandler := handlers.NewWebSocketHandler(deps.Hub, cfg.CORS.AllowedOrigins)
	if a == nil {
		// WebSocket endpoint (top-level, outside the rate limiter: connections
		// are long-lived).
		router.GET("/ws", wsHandler.HandleWebSocket)
	} else {
		a.registerWebSocket(router, wsHandler, limiters.API)
	}

	// Health check endpoints
	healthGroup := router.Group("/health")
	{
		healthGroup.GET("/live", handlers.LivenessHandler(deps.HealthChecker))
		healthGroup.GET("/ready", handlers.ReadinessHandler(deps.HealthChecker))
		healthGroup.GET("", handlers.HealthCheck) // Keep the original health check for backward compatibility
	}

	v1 := router.Group("/api/v1")
	v1.Use(limiters.API.RateLimit())

	// Public UI config (no auth, so the login page can read it)
	v1.GET("/ui-config", handlers.NewUIConfigHandler(handlers.UIConfigFromConfig(cfg)))

	// The routes of the API: open with auth off, behind CombinedAuth and the
	// audit middleware with auth on.
	api := v1
	result := &Result{RateLimiters: limiters}
	if a != nil {
		limiters.Register = handlers.NewRateLimiter(int(cfg.Server.LoginRateLimit), time.Minute)
		limiters.OIDCLogin = handlers.NewRateLimiter(int(cfg.Server.LoginRateLimit), time.Minute)
		limiters.OIDCCallback = handlers.NewRateLimiter(int(cfg.Server.LoginRateLimit), time.Minute)
		limiters.PasswordChange = handlers.NewRateLimiter(int(cfg.Server.LoginRateLimit), time.Minute)
		a.registerPublicAuth(v1, limiters)
		authMW := []gin.HandlerFunc{a.authenticate}
		if cfg.Otel.Enabled {
			// The span of an authenticated request names its user.
			authMW = append(authMW, middleware.SpanEnrichUser())
		}
		authMW = append(authMW, a.recorder.Middleware())
		api = v1.Group("", authMW...)
		a.registerAuthenticated(api, limiters)
		result.AuthHandler = a.authHandler
		result.AuditRecorder = a.recorder
	}

	// Ping endpoint
	api.GET("/ping", handlers.Ping)

	// Items endpoints
	itemsHandler := handlers.NewHandlerWithHub(deps.Repository, deps.Hub)
	items := api.Group("/items")
	{
		items.GET("", itemsHandler.GetItems)
		items.GET("/:id", itemsHandler.GetItem)
		items.POST("", itemsHandler.CreateItem)
		items.PUT("/:id", itemsHandler.UpdateItem)
		items.DELETE("/:id", itemsHandler.DeleteItem)
	}

	return result, nil
}

// setTrustedProxies sets the proxies that may set the client IP. Only the
// proxies of TRUSTED_PROXIES may set it with X-Forwarded-For or X-Real-IP.
// Gin trusts every peer by default, so a client could pick a new IP for each
// request and pass the per-IP rate limits. A nil list trusts no proxy: the
// client IP is the TCP peer. config.ParseTrustedProxies validated the list;
// on an error, trust no proxy.
func setTrustedProxies(router *gin.Engine, trustedProxies []string) {
	if len(trustedProxies) == 0 {
		trustedProxies = nil
	}
	if err := router.SetTrustedProxies(trustedProxies); err != nil {
		slog.Error("Invalid TRUSTED_PROXIES; no proxy is trusted", "error", err)
		_ = router.SetTrustedProxies(nil)
	}
}

// authWiring holds the auth handlers and middleware of an auth-on router.
type authWiring struct {
	deps         *AuthDeps
	hub          *websocket.Hub
	authHandler  *handlers.AuthHandler
	oidcHandler  *handlers.OIDCHandler
	apiKeys      *handlers.APIKeyHandler
	users        *handlers.UserHandler
	auditLogs    *handlers.AuditLogHandler
	recorder     *middleware.AuditRecorder
	authenticate gin.HandlerFunc
}

// newAuthWiring builds the auth handlers. It starts nothing, so an error
// leaves nothing to stop.
func newAuthWiring(cfg *config.Config, d *AuthDeps, hub *websocket.Hub) (*authWiring, error) {
	provider := d.OIDCProvider
	if isNil(provider) && cfg.OIDC.Enabled {
		p, err := auth.NewProvider(&cfg.OIDC, auth.Options{})
		if err != nil {
			return nil, fmt.Errorf("oidc provider: %w", err)
		}
		provider = p
	}

	// The same OIDC config for the password login (OIDC-only refuses a
	// password login of a non-service account) and the OIDC routes. The
	// refresh repository is set before the OIDC handler is built: without
	// it the OIDC routes answer 501.
	authHandler := handlers.NewAuthHandler(d.Users, &cfg.Auth, &cfg.OIDC)
	authHandler.SetRefreshTokenRepo(d.RefreshTokens)
	authHandler.SetSessionStore(d.SessionStore)

	// R20: production fails closed when a revocation lookup fails.
	keyAuthDeps := middleware.APIKeyAuthDeps{
		JWTSecret:              cfg.Auth.JWTSecret,
		APIKeyRepo:             d.APIKeys,
		UserRepo:               d.Users,
		SessionStore:           d.SessionStore,
		FailClosedOnStoreError: true,
	}

	w := &authWiring{
		deps:         d,
		hub:          hub,
		authHandler:  authHandler,
		oidcHandler:  handlers.NewOIDCHandler(provider, authHandler, d.Users, d.SessionStore, &cfg.OIDC),
		apiKeys:      handlers.NewAPIKeyHandler(d.APIKeys, d.Users, handlers.APIKeyOptions{}),
		users:        handlers.NewUserHandler(d.Users, d.RefreshTokens, d.APIKeys, &cfg.Auth),
		auditLogs:    handlers.NewAuditLogHandler(d.AuditLogs, d.Users),
		recorder:     middleware.NewAuditRecorder(d.AuditLogs),
		authenticate: middleware.CombinedAuth(keyAuthDeps),
	}
	w.users.SetSessionStore(d.SessionStore)
	if hub != nil {
		// Logout, logout-all, user revocations and key deletes close the
		// open sockets of this replica.
		authHandler.SetWebSocketRevoker(hub)
		w.users.SetWebSocketRevoker(hub)
		w.apiKeys.SetWebSocketRevoker(hub)
	}
	return w, nil
}

// registerWebSocket registers the authenticated /ws upgrade and starts the
// revalidation of the open sockets (it stops when the hub shuts down).
func (a *authWiring) registerWebSocket(router *gin.Engine, ws *handlers.WebSocketHandler, apiLimiter *handlers.RateLimiter) {
	check := handlers.NewWebSocketRevocationChecker(a.deps.SessionStore, a.deps.Users, a.deps.APIKeys, true)
	ws.SetIdentityResolver(handlers.WebSocketIdentityFromContext)
	ws.SetRecheck(check)
	if a.hub != nil {
		a.hub.SetTopicAuthorizer(handlers.DefaultTopicAuthorizer)
		interval := a.deps.RevalidateInterval
		if interval <= 0 {
			interval = websocket.DefaultRevalidateInterval
		}
		a.hub.StartRevalidation(interval, check)
	}
	router.GET("/ws",
		apiLimiter.RateLimit(),
		middleware.WebSocketQueryToken(),
		a.authenticate,
		middleware.RequireActiveUser(a.deps.Users),
		ws.HandleWebSocket,
	)
}

// registerPublicAuth registers the auth routes that authenticate themselves.
func (a *authWiring) registerPublicAuth(v1 *gin.RouterGroup, l *RateLimiters) {
	g := v1.Group("/auth")
	g.POST("/login", l.Login.RateLimit(), a.authHandler.Login)
	g.POST("/register", l.Register.RateLimit(), a.authHandler.Register)
	// No dedicated limiter: a refresh needs a valid token, and several tabs
	// refresh often. It stays under the v1 API limiter (RATE_LIMIT).
	g.POST("/refresh", a.authHandler.Refresh)
	// Public: a client with an expired access token can still end its session
	// with the cookie. RequireSessionAuth passes here (no auth middleware ran);
	// the handler reads only the bearer token and the cookie.
	g.POST("/logout", middleware.RequireSessionAuth(), a.authHandler.Logout)
	g.GET("/oidc/login", l.OIDCLogin.RateLimit(), a.oidcHandler.Login)
	g.GET("/oidc/callback", l.OIDCCallback.RateLimit(), a.oidcHandler.Callback)
}

// registerAuthenticated registers the routes behind CombinedAuth.
func (a *authWiring) registerAuthenticated(api *gin.RouterGroup, l *RateLimiters) {
	g := api.Group("/auth")
	g.GET("/me", a.authHandler.GetCurrentUser)
	// Login session only (an API key is refused with 403). The audit table has
	// an entry for it: the group is under /auth/, which is otherwise not
	// audited.
	g.PUT("/me/password", l.PasswordChange.RateLimit(), middleware.RequireSessionAuth(), a.authHandler.ChangePassword)
	g.POST("/logout-all", middleware.RequireSessionAuth(), a.authHandler.LogoutAll)

	keys := api.Group("/api-keys", middleware.RequireSessionAuth())
	keys.POST("", a.apiKeys.CreateAPIKey)
	keys.GET("", a.apiKeys.ListAPIKeys)
	keys.DELETE("/:id", a.apiKeys.DeleteAPIKey)

	// The handlers check the admin role and the login session themselves (also
	// against the database); the route gates are a second layer.
	users := api.Group("/users", middleware.RequireSessionAuth(), middleware.RequireAdmin())
	users.GET("", a.users.ListUsers)
	users.POST("", a.users.CreateUser)
	users.GET("/:id", a.users.GetUser)
	users.PUT("/:id", a.users.UpdateUser)
	users.DELETE("/:id", a.users.DeleteUser)
	users.PUT("/:id/role", a.users.ChangeUserRole)
	users.PUT("/:id/disabled", a.users.SetUserDisabled)

	api.GET("/audit-logs", middleware.RequireSessionAuth(), middleware.RequireAdmin(), a.auditLogs.ListAuditLogs)
}
