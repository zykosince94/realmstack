// Package httpapi is the HTTP layer: the Gin engine, cross-cutting middleware,
// authentication, and the route table that wires URLs to domain handlers. Domain
// packages hold the logic; this package decides who may call what.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"realmstack-backend/internal/ai"
	"realmstack-backend/internal/bans"
	"realmstack-backend/internal/leaderboards"
	"realmstack-backend/internal/limits"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/players"
)

// Config is the runtime configuration read by cmd/api.
type Config struct {
	Env        string
	JWTSecret  string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	BodyLimit  int64
	DemoMode   bool
	// TrustedProxies lists proxy IPs/CIDRs whose X-Forwarded-For header is honoured
	// when resolving the client IP. Empty means the TCP peer address is always used.
	TrustedProxies []string
	// AuthRateLimit is the number of authentication attempts allowed per client IP,
	// per route, per minute. Zero uses the default of 30.
	AuthRateLimit int
	// AI configures the optional AI assistant; an empty encryption key disables it.
	AI ai.Config
	// Quotas are the per-tenant defaults for projects without a project_quotas row.
	// Zero fields use DefaultQuotas.
	Quotas limits.Limits
	// CredentialCacheTTL caches successful server-credential lookups (capped at 60 s;
	// 0 uses 30 s, negative disables).
	CredentialCacheTTL time.Duration
	// BanSyncInterval is how often each instance reads ban changes made elsewhere, so it
	// bounds how long a newly banned player's access token keeps working on another
	// instance. 0 uses bans.DefaultSyncInterval (5 s).
	BanSyncInterval time.Duration
}

// DefaultQuotas leave headroom above the spec's target tenant (about 5,000 writes and
// a similar number of reads per second).
var DefaultQuotas = limits.Limits{RequestsPerMinute: 1_200_000, WritesPerMinute: 600_000, MaxBatchEntries: 500}

// Server owns the Gin engine and the shared Env.
type Server struct {
	Env     *platform.Env
	AI      *ai.Service
	Quotas  *limits.Quotas // nil without a database (unit tests)
	Bans    *bans.Cache    // every ban in force, checked on each player-token request
	cfg     Config
	router  *gin.Engine
	limiter *rateLimiter
}

// New builds the server and its routes.
func New(db *pgxpool.Pool, cfg Config, logger *slog.Logger) *Server {
	if cfg.BodyLimit <= 0 {
		cfg.BodyLimit = 1 << 20
	}
	if cfg.AuthRateLimit <= 0 {
		cfg.AuthRateLimit = 30
	}
	if cfg.Quotas.RequestsPerMinute <= 0 {
		cfg.Quotas.RequestsPerMinute = DefaultQuotas.RequestsPerMinute
	}
	if cfg.Quotas.WritesPerMinute <= 0 {
		cfg.Quotas.WritesPerMinute = DefaultQuotas.WritesPerMinute
	}
	if cfg.Quotas.MaxBatchEntries <= 0 {
		cfg.Quotas.MaxBatchEntries = DefaultQuotas.MaxBatchEntries
	}
	if cfg.CredentialCacheTTL == 0 {
		cfg.CredentialCacheTTL = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	gin.SetMode(gin.ReleaseMode)
	s := &Server{
		Env:     &platform.Env{DB: db, Log: logger, JWTSecret: cfg.JWTSecret, AccessTTL: cfg.AccessTTL, RefreshTTL: cfg.RefreshTTL, Now: time.Now, SecureCookies: cfg.Env != "local"},
		cfg:     cfg,
		router:  gin.New(),
		limiter: newRateLimiter(cfg.AuthRateLimit, time.Minute),
	}
	clock := func() time.Time { return s.Env.Now() } // follows tests that replace Env.Now
	s.Env.Credentials = platform.NewCredentialCache(cfg.CredentialCacheTTL, clock)
	s.Bans = bans.NewCache(db, logger, clock, cfg.BanSyncInterval)
	if db != nil {
		s.Quotas = limits.NewQuotas(db, cfg.Quotas, clock)
	}
	assistant, err := ai.NewService(s.Env, cfg.AI)
	if err != nil {
		// cmd/api validates the key before starting; reaching here means a programming
		// error, so fail safe: run with the assistant disabled.
		logger.Error("AI assistant disabled: invalid configuration", "error", err)
		assistant, _ = ai.NewService(s.Env, ai.Config{})
	}
	s.AI = assistant
	// Gin trusts every proxy by default, which would let any client spoof X-Forwarded-For.
	if err := s.router.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		logger.Error("invalid trusted proxy configuration; trusting no proxies", "error", err)
		_ = s.router.SetTrustedProxies(nil)
	}
	s.router.Use(s.recovery(), s.requestContext(), s.limitBody(), validateIDParams())
	s.routes()
	s.router.NoRoute(func(c *gin.Context) {
		platform.Fail(c, http.StatusNotFound, "NOT_FOUND", "The requested route was not found.")
	})
	return s
}

// Start runs the server's background work until ctx ends: syncing quota counters
// across instances every 250 ms.
func (s *Server) Start(ctx context.Context) {
	if s.Quotas != nil {
		go s.Quotas.Run(ctx, 250*time.Millisecond, func(err error) { s.Env.Log.Error("quota sync failed", "error", err) })
	}
	// The rank projection drains the outbox every 100 ms when idle and repairs boards
	// that are not ready (new boards, or Redis lost its data) every 30 s.
	if s.Env.Redis != nil {
		p := &leaderboards.Projection{DB: s.Env.DB, Redis: s.Env.Redis, Log: s.Env.Log}
		go p.Run(ctx, 100*time.Millisecond, 30*time.Second)
	}
}

// Handler exposes the HTTP handler, mainly for tests.
func (s *Server) Handler() http.Handler { return s.router }

// Routes lists the registered method and path pairs (used by the OpenAPI drift test).
func (s *Server) Routes() gin.RoutesInfo { return s.router.Routes() }

// HTTPServer returns an http.Server with timeouts that protect against slow clients.
// The caller owns ListenAndServe and Shutdown.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// ParseIntEnv reads a positive integer environment variable, or returns fallback.
func ParseIntEnv(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

var requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func (s *Server) requestContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if len(id) < 8 || len(id) > 80 || !requestIDPattern.MatchString(id) {
			id = platform.RandomUUID()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		start := time.Now()
		c.Next()
		s.Env.Log.Info("http request", "request_id", id, "method", c.Request.Method, "path", c.FullPath(), "status", c.Writer.Status(), "duration_ms", time.Since(start).Milliseconds())
	}
}

func (s *Server) recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.Env.Log.Error("request panic", "request_id", c.GetString("request_id"), "error", fmt.Sprint(recovered))
				if !c.Writer.Written() {
					platform.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "An unexpected error occurred.")
				}
			}
		}()
		c.Next()
	}
}

func (s *Server) limitBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.cfg.BodyLimit)
		c.Next()
	}
}

// validateIDParams answers 404 for any malformed UUID route parameter, so a bad ID
// never reaches a query (where the uuid cast would fail as a 500).
func validateIDParams() gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, p := range c.Params {
			switch p.Key {
			case "projectId", "envId", "id", "playerId":
				if !platform.UUIDPattern.MatchString(p.Value) {
					platform.NotFound(c)
					return
				}
			}
		}
		c.Next()
	}
}

// userAuth and playerAuth require a signed, unexpired JWT of their kind.
func (s *Server) userAuth() gin.HandlerFunc   { return s.jwtAuth(platform.KindUser) }
func (s *Server) playerAuth() gin.HandlerFunc { return s.jwtAuth(platform.KindPlayer) }

func (s *Server) jwtAuth(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := s.Env.ReadToken(c.GetHeader("Authorization"), kind)
		if !ok {
			platform.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "A valid access token is required.")
			return
		}
		if kind == platform.KindPlayer && s.refuseBanned(c, p.ID) {
			return
		}
		platform.SetPrincipal(c, p)
		c.Next()
	}
}

// refuseBanned answers 403 PLAYER_BANNED (and reports true) for a banned player. Access
// tokens are stateless and valid for up to 15 minutes, so without this check a ban would
// only take effect when the token expired.
func (s *Server) refuseBanned(c *gin.Context, playerID string) bool {
	ban, err := s.Bans.Check(c, playerID)
	if err != nil {
		platform.Internal(c, err)
		return true
	}
	if ban != nil {
		bans.Denied(c, ban)
		return true
	}
	return false
}

// serverAuth authenticates X-Server-Credential: prefix.secret. The prefix selects the
// credential row; the secret is compared as a digest in constant time. Only credentials
// of active projects pass.
func (s *Server) serverAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := strings.TrimSpace(c.GetHeader("X-Server-Credential"))
		prefix, secret, found := strings.Cut(key, ".")
		if key == "" || !found {
			platform.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "A valid server credential is required.")
			return
		}
		p := platform.Principal{ID: prefix, Kind: platform.KindServer}
		// A cached lookup skips the query, never the secret check: the digest is
		// compared on every request.
		cached, ok := s.Env.Credentials.Get(prefix)
		if !ok {
			err := s.Env.DB.QueryRow(c, `SELECT sc.project_id::text,sc.environment_id::text,sc.secret_hash FROM server_credentials sc JOIN projects pr ON pr.id=sc.project_id
				WHERE sc.key_prefix=$1 AND sc.revoked_at IS NULL AND pr.status='active'`, prefix).Scan(&cached.ProjectID, &cached.EnvironmentID, &cached.SecretHash)
			if err != nil {
				platform.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "The server credential is invalid.")
				return
			}
		}
		if !platform.SecureHash(cached.SecretHash, secret) {
			platform.Fail(c, http.StatusUnauthorized, "UNAUTHORIZED", "The server credential is invalid.")
			return
		}
		if !ok {
			s.Env.Credentials.Put(prefix, cached) // only lookups whose secret matched are cached
		}
		p.ProjectID, p.EnvironmentID = cached.ProjectID, cached.EnvironmentID
		platform.SetPrincipal(c, p)
		c.Next()
	}
}

// publicScope accepts a player token (which also identifies the player) or a public
// environment identifier from the X-Public-Environment header or ?environment_id=.
func (s *Server) publicScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p, ok := s.Env.ReadToken(c.GetHeader("Authorization"), platform.KindPlayer); ok {
			if s.refuseBanned(c, p.ID) {
				return
			}
			platform.SetPrincipal(c, p)
			platform.SetScope(c, p)
			c.Next()
			return
		}
		public := c.GetHeader("X-Public-Environment")
		if public == "" {
			public = c.Query("environment_id")
		}
		p, ok := players.LookupPublic(c, s.Env.DB, public)
		if !ok {
			return
		}
		p.Kind = platform.KindPublic
		platform.SetScope(c, p)
		c.Next()
	}
}
