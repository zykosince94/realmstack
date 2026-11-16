package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

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
}

type Server struct {
	db      *pgxpool.Pool
	cfg     Config
	log     *slog.Logger
	router  *gin.Engine
	limiter *rateLimiter
	// now is the single clock for token expiry, refresh expiry and score timestamps.
	// Tests replace it to exercise boundaries.
	now func() time.Time
}

type principal struct {
	ID, ProjectID, EnvironmentID, Kind string
}

func New(db *pgxpool.Pool, cfg Config, logger *slog.Logger) *Server {
	if cfg.BodyLimit <= 0 {
		cfg.BodyLimit = 1 << 20
	}
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.AuthRateLimit <= 0 {
		cfg.AuthRateLimit = 30
	}
	gin.SetMode(gin.ReleaseMode)
	s := &Server{db: db, cfg: cfg, log: logger, router: gin.New(), limiter: newRateLimiter(cfg.AuthRateLimit, time.Minute), now: time.Now}
	// Gin trusts every proxy by default, which would let any client spoof X-Forwarded-For.
	// Only the configured proxies are trusted; with none, ClientIP is the TCP peer.
	if err := s.router.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		logger.Error("invalid trusted proxy configuration; trusting no proxies", "error", err)
		_ = s.router.SetTrustedProxies(nil)
	}
	s.router.Use(s.recovery(), s.requestContext(), s.limitBody(), validateIDParams())
	s.routes()
	s.router.NoRoute(func(c *gin.Context) {
		failure(c, http.StatusNotFound, "NOT_FOUND", "The requested route was not found.")
	})
	s.router.NoMethod(func(c *gin.Context) {
		failure(c, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "The method is not allowed for this route.")
	})
	return s
}

// Handler exposes the HTTP handler, mainly for tests.
func (s *Server) Handler() http.Handler { return s.router }

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

func ParseIntEnv(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

// Migrate applies migrations from ./migrations (relative to the working directory).
func Migrate(ctx context.Context, db *pgxpool.Pool) error {
	return MigrateDir(ctx, db, "migrations")
}

// migrationLockID is the PostgreSQL advisory lock key held while migrating.
const migrationLockID = 72_656_176_000_001

// MigrateDir applies every *.sql file in dir, in lexical order, exactly once. A
// session advisory lock serializes API instances that start at the same time.
func MigrateDir(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	db, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer db.Release()
	if _, err = db.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() { _, _ = db.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockID) }()
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	for _, file := range files {
		version := filepath.Base(file)
		var applied bool
		if err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied {
			continue
		}
		b, readErr := os.ReadFile(file)
		if readErr != nil {
			return fmt.Errorf("read migration %s: %w", version, readErr)
		}
		tx, beginErr := db.Begin(ctx)
		if beginErr != nil {
			return fmt.Errorf("begin migration %s: %w", version, beginErr)
		}
		if _, err = tx.Exec(ctx, string(b)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, version)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}
	return nil
}

func (s *Server) requestContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if len(id) < 8 || len(id) > 80 || !requestIDPattern.MatchString(id) {
			id = randomUUID()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		start := time.Now()
		c.Next()
		s.log.Info("http request", "request_id", id, "method", c.Request.Method, "path", c.FullPath(), "status", c.Writer.Status(), "duration_ms", time.Since(start).Milliseconds())
	}
}

func (s *Server) recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("request panic", "request_id", c.GetString("request_id"), "error", fmt.Sprint(recovered))
				if !c.Writer.Written() {
					failure(c, http.StatusInternalServerError, "INTERNAL_ERROR", "An unexpected error occurred.")
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

func (s *Server) routes() {
	r := s.router
	r.GET("/healthz", func(c *gin.Context) { success(c, http.StatusOK, gin.H{"status": "ok"}) })
	v := r.Group("/v1")
	v.POST("/auth/register", s.rateLimit(), s.registerUser)
	v.POST("/auth/login", s.rateLimit(), s.loginUser)
	v.GET("/projects", s.userAuth(), s.listProjects)
	v.POST("/projects", s.userAuth(), s.createProject)
	v.GET("/projects/:projectId", s.userAuth(), s.getProject)
	v.PATCH("/projects/:projectId", s.userAuth(), s.updateProject)
	v.GET("/projects/:projectId/environments", s.userAuth(), s.listEnvironments)
	v.POST("/projects/:projectId/environments", s.userAuth(), s.createEnvironment)
	v.GET("/projects/:projectId/environments/:envId/credentials", s.userAuth(), s.listCredentials)
	v.POST("/projects/:projectId/environments/:envId/credentials", s.userAuth(), s.issueCredential)
	v.POST("/credentials/:id/revoke", s.userAuth(), s.revokeCredential)
	v.GET("/projects/:projectId/environments/:envId/configurations/draft", s.userAuth(), s.getDraft)
	v.PUT("/projects/:projectId/environments/:envId/configurations/draft", s.userAuth(), s.putDraft)
	v.POST("/projects/:projectId/environments/:envId/configurations/publish", s.userAuth(), s.publishConfig)
	v.GET("/projects/:projectId/environments/:envId/configurations/versions", s.userAuth(), s.listConfigVersions)
	v.GET("/projects/:projectId/environments/:envId/configurations/published", s.userAuth(), s.getPublishedConfig)
	v.POST("/projects/:projectId/environments/:envId/configurations/validate", s.userAuth(), s.validateDraft)
	for _, name := range draftCollections {
		kind := definitionKinds[name]
		base := "/projects/:projectId/environments/:envId/" + name
		v.GET(base, s.userAuth(), s.listDefinitions(kind))
		v.POST(base, s.userAuth(), s.createDefinition(kind))
		v.GET(base+"/:key", s.userAuth(), s.getDefinition(kind))
		v.PUT(base+"/:key", s.userAuth(), s.replaceDefinition(kind))
		v.DELETE(base+"/:key", s.userAuth(), s.deleteDefinition(kind))
	}
	v.GET("/projects/:projectId/environments/:envId/progression", s.userAuth(), s.getDraftProgression)
	v.PUT("/projects/:projectId/environments/:envId/progression", s.userAuth(), s.putDraftProgression)
	v.POST("/players/register", s.rateLimit(), s.registerPlayer)
	v.POST("/players/login", s.rateLimit(), s.loginPlayer)
	v.POST("/players/refresh", s.refreshPlayer)
	v.GET("/players/me", s.playerAuth(), s.getMe)
	v.PATCH("/players/me", s.playerAuth(), s.patchMe)
	v.GET("/players/me/inventory", s.playerAuth(), s.getInventory)
	v.GET("/players/me/inventory/:itemKey", s.playerAuth(), s.getInventoryItem)
	v.POST("/players/me/inventory/consume", s.playerAuth(), s.consumeItem)
	v.POST("/players/me/inventory/equip", s.playerAuth(), s.equipItem)
	v.POST("/players/me/inventory/unequip", s.playerAuth(), s.unequipItem)
	v.GET("/players/me/currency", s.playerAuth(), s.getBalances)
	v.POST("/players/me/currency/spend", s.playerAuth(), s.spendCurrency)
	v.GET("/players/me/transactions", s.playerAuth(), s.getTransactions)
	v.GET("/players/me/progression", s.playerAuth(), s.getProgression)
	v.POST("/server/players/:playerId/items/grant", s.serverAuth(), s.grantItem)
	v.POST("/server/players/:playerId/currency/credit", s.serverAuth(), s.creditCurrency)
	v.POST("/server/players/:playerId/xp/award", s.serverAuth(), s.awardXP)
	v.POST("/leaderboards/:key/scores", s.playerAuth(), s.submitScore)
	v.GET("/leaderboards/:key", s.publicScope(), s.getLeaderboard)
}

func success(c *gin.Context, status int, data any) {
	successMeta(c, status, data, nil)
}

// successMeta adds extra fields (such as a pagination cursor) to the response meta.
func successMeta(c *gin.Context, status int, data any, extra gin.H) {
	meta := gin.H{"request_id": c.GetString("request_id")}
	for k, v := range extra {
		meta[k] = v
	}
	c.JSON(status, gin.H{"data": data, "meta": meta})
}
func failure(c *gin.Context, status int, code, message string) {
	failureDetails(c, status, code, message, nil)
}

// failureDetails sends the standard error envelope; details defaults to {}.
func failureDetails(c *gin.Context, status int, code, message string, details any) {
	if details == nil {
		details = gin.H{}
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message, "details": details}, "meta": gin.H{"request_id": c.GetString("request_id")}})
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateIDParams answers 404 for any malformed UUID route parameter, so a bad ID
// never reaches a query (where the uuid cast would fail as a 500).
func validateIDParams() gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, p := range c.Params {
			switch p.Key {
			case "projectId", "envId", "id", "playerId":
				if !uuidPattern.MatchString(p.Value) {
					notFound(c)
					return
				}
			}
		}
		c.Next()
	}
}
func badRequest(c *gin.Context) {
	failure(c, 400, "INVALID_REQUEST", "The request body or fields are invalid.")
}
func notFound(c *gin.Context) { failure(c, 404, "NOT_FOUND", "The requested resource was not found.") }
func internal(c *gin.Context, err error) {
	slog.Error("request failed", "request_id", c.GetString("request_id"), "error", err)
	failure(c, 500, "INTERNAL_ERROR", "An unexpected error occurred.")
}
func bind(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		badRequest(c)
		return false
	}
	return true
}
func (s *Server) userAuth() gin.HandlerFunc   { return s.jwtAuth("user") }
func (s *Server) playerAuth() gin.HandlerFunc { return s.jwtAuth("player") }
func (s *Server) jwtAuth(kind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := s.readJWT(c.GetHeader("Authorization"), kind)
		if !ok {
			failure(c, 401, "UNAUTHORIZED", "A valid access token is required.")
			return
		}
		c.Set("principal", p)
		c.Next()
	}
}

func (s *Server) readJWT(header, kind string) (principal, bool) {
	var p principal
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return p, false
	}
	t := strings.Split(parts[1], ".")
	if len(t) != 3 {
		return p, false
	}
	signature := hmacText(s.cfg.JWTSecret, t[0]+"."+t[1])
	if subtle.ConstantTimeCompare([]byte(signature), []byte(t[2])) != 1 {
		return p, false
	}
	b, err := base64.RawURLEncoding.DecodeString(t[1])
	if err != nil {
		return p, false
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return p, false
	}
	exp, ok := claims["exp"].(float64)
	if !ok || s.now().Unix() >= int64(exp) {
		return p, false
	}
	p.ID, _ = claims["sub"].(string)
	p.ProjectID, _ = claims["project_id"].(string)
	p.EnvironmentID, _ = claims["environment_id"].(string)
	p.Kind, _ = claims["kind"].(string)
	return p, p.ID != "" && p.Kind == kind
}
func (s *Server) token(p principal, ttl time.Duration) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(gin.H{"sub": p.ID, "kind": p.Kind, "project_id": p.ProjectID, "environment_id": p.EnvironmentID, "exp": s.now().Add(ttl).Unix()})
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + hmacText(s.cfg.JWTSecret, body)
}
func hmacText(secret, msg string) string {
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (s *Server) serverAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := strings.TrimSpace(c.GetHeader("X-Server-Credential"))
		if key == "" {
			failure(c, 401, "UNAUTHORIZED", "A server credential is required.")
			return
		}
		parts := strings.SplitN(key, ".", 2)
		if len(parts) != 2 {
			failure(c, 401, "UNAUTHORIZED", "The server credential is invalid.")
			return
		}
		var p principal
		var hash string
		err := s.db.QueryRow(c, `SELECT sc.project_id::text,sc.environment_id::text,sc.secret_hash FROM server_credentials sc JOIN projects pr ON pr.id=sc.project_id WHERE sc.key_prefix=$1 AND sc.revoked_at IS NULL AND pr.status='active'`, parts[0]).Scan(&p.ProjectID, &p.EnvironmentID, &hash)
		if err != nil || !secureHash(hash, parts[1]) {
			failure(c, 401, "UNAUTHORIZED", "The server credential is invalid.")
			return
		}
		p.Kind = "server"
		p.ID = parts[0]
		c.Set("principal", p)
		c.Next()
	}
}
func (s *Server) publicScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		if p, ok := s.readJWT(c.GetHeader("Authorization"), "player"); ok {
			c.Set("principal", p)
			c.Set("scope", p)
			c.Next()
			return
		}
		public := c.GetHeader("X-Public-Environment")
		if public == "" {
			public = c.Query("environment_id")
		}
		p, ok := s.lookupPublic(c, public)
		if !ok {
			return
		}
		p.Kind = "public"
		c.Set("scope", p)
		c.Next()
	}
}

// lookupPublic resolves a public environment identifier of an active project. It
// accepts only the public identifier, never the internal environment UUID.
func (s *Server) lookupPublic(c *gin.Context, public string) (principal, bool) {
	var p principal
	err := s.db.QueryRow(c, `SELECT e.project_id::text,e.id::text FROM environments e JOIN projects pr ON pr.id=e.project_id WHERE e.public_id=$1 AND pr.status='active'`, public).Scan(&p.ProjectID, &p.EnvironmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		failure(c, 404, "NOT_FOUND", "The public project environment was not found.")
		return p, false
	}
	if err != nil {
		internal(c, err)
		return p, false
	}
	return p, true
}
func getPrincipal(c *gin.Context) principal { p, _ := c.Get("principal"); return p.(principal) }
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}
func randomSecret(n int) string {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		return randomID()
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func secureHash(stored, raw string) bool {
	return subtle.ConstantTimeCompare([]byte(stored), []byte(digest(raw))) == 1
}
func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,48}$`)

func validSlug(s string) bool { return slugPattern.MatchString(s) }

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

var (
	dummyHashOnce sync.Once
	dummyHash     []byte
)

// checkPassword compares a password against a stored bcrypt hash. When the account
// does not exist (found == false) it still runs a bcrypt comparison against a dummy
// hash, so response timing does not reveal whether an email is registered.
func checkPassword(found bool, hash, password string) bool {
	if !found {
		dummyHashOnce.Do(func() {
			dummyHash, _ = bcrypt.GenerateFromPassword([]byte("realmstack-timing-equaliser"), bcrypt.DefaultCost)
		})
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
func ownerProject(c *gin.Context, db *pgxpool.Pool, projectID string) bool {
	p := getPrincipal(c)
	var exists bool
	err := db.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND owner_user_id=$2)`, projectID, p.ID).Scan(&exists)
	return err == nil && exists
}
func (s *Server) ownedEnvironment(c *gin.Context) (string, string, bool) {
	projectID, envID := c.Param("projectId"), c.Param("envId")
	if !ownerProject(c, s.db, projectID) {
		notFound(c)
		return "", "", false
	}
	var exists bool
	err := s.db.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM environments WHERE id=$1 AND project_id=$2)`, envID, projectID).Scan(&exists)
	if err != nil || !exists {
		notFound(c)
		return "", "", false
	}
	return projectID, envID, true
}

func (s *Server) registerUser(c *gin.Context) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !bind(c, &in) {
		return
	}
	in.Email = normalizeEmail(in.Email)
	if len(in.Email) < 5 || len(in.Email) > 254 || len(in.Password) < 10 || len(in.Password) > 128 {
		failure(c, 422, "VALIDATION_ERROR", "Email or password does not meet requirements.")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		internal(c, err)
		return
	}
	var id string
	err = s.db.QueryRow(c, `INSERT INTO users(email,password_hash) VALUES($1,$2) RETURNING id::text`, in.Email, string(hash)).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			failure(c, 409, "CONFLICT", "An account with that email already exists.")
			return
		}
		internal(c, err)
		return
	}
	success(c, 201, gin.H{"user": gin.H{"id": id, "email": in.Email}, "access_token": s.token(principal{ID: id, Kind: "user"}, s.cfg.AccessTTL), "token_type": "Bearer"})
}
func (s *Server) loginUser(c *gin.Context) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !bind(c, &in) {
		return
	}
	var id, hash string
	err := s.db.QueryRow(c, `SELECT id::text,password_hash FROM users WHERE email=$1`, normalizeEmail(in.Email)).Scan(&id, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		internal(c, err)
		return
	}
	if !checkPassword(err == nil, hash, in.Password) {
		failure(c, 401, "INVALID_CREDENTIALS", "Email or password is incorrect.")
		return
	}
	success(c, 200, gin.H{"user": gin.H{"id": id, "email": normalizeEmail(in.Email)}, "access_token": s.token(principal{ID: id, Kind: "user"}, s.cfg.AccessTTL), "token_type": "Bearer"})
}

// projectView is the API shape of a project row.
func projectView(id, name, slug, status string, created time.Time, archived *time.Time) gin.H {
	out := gin.H{"id": id, "name": name, "slug": slug, "status": status, "created_at": created}
	if archived != nil {
		out["archived_at"] = archived
	}
	return out
}

// listProjects lists the caller's active projects; ?include_archived=true adds archived ones.
func (s *Server) listProjects(c *gin.Context) {
	p := getPrincipal(c)
	includeArchived := c.Query("include_archived") == "true"
	rows, err := s.db.Query(c, `SELECT id::text,name,slug,status,created_at,archived_at FROM projects WHERE owner_user_id=$1 AND ($2 OR status='active') ORDER BY created_at`, p.ID, includeArchived)
	if err != nil {
		internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var id, name, slug, status string
		var created time.Time
		var archived *time.Time
		if err = rows.Scan(&id, &name, &slug, &status, &created, &archived); err != nil {
			internal(c, err)
			return
		}
		out = append(out, projectView(id, name, slug, status, created, archived))
	}
	if err = rows.Err(); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, out)
}
func (s *Server) createProject(c *gin.Context) {
	var in struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !bind(c, &in) {
		return
	}
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) < 1 || len(in.Name) > 100 || !validSlug(in.Slug) {
		failure(c, 422, "VALIDATION_ERROR", "A project name and valid slug are required.")
		return
	}
	p := getPrincipal(c)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var id, envID, public string
	if err = tx.QueryRow(c, `INSERT INTO projects(owner_user_id,name,slug) VALUES($1,$2,$3) RETURNING id::text`, p.ID, in.Name, in.Slug).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			failure(c, 409, "CONFLICT", "Project slug is already in use.")
			return
		}
		internal(c, err)
		return
	}
	public = "pub_" + randomSecret(18)
	if err = tx.QueryRow(c, `INSERT INTO environments(project_id,name,public_id) VALUES($1,'development',$2) RETURNING id::text`, id, public).Scan(&envID); err != nil {
		internal(c, err)
		return
	}
	_, err = tx.Exec(c, `INSERT INTO configuration_drafts(project_id,environment_id) VALUES($1,$2)`, id, envID)
	if err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 201, gin.H{"id": id, "name": in.Name, "slug": in.Slug, "status": "active", "environments": []gin.H{{"id": envID, "name": "development", "public_id": public}}})
}
func (s *Server) getProject(c *gin.Context) {
	p := getPrincipal(c)
	var id, name, slug, status string
	var created time.Time
	var archived *time.Time
	err := s.db.QueryRow(c, `SELECT id::text,name,slug,status,created_at,archived_at FROM projects WHERE id=$1 AND owner_user_id=$2`, c.Param("projectId"), p.ID).Scan(&id, &name, &slug, &status, &created, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		notFound(c)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	success(c, 200, projectView(id, name, slug, status, created, archived))
}

// updateProject renames a project, changes its slug, or archives/restores it with
// {"status":"archived"|"active"}. Projects are never deleted: archiving keeps every
// ledger and stops the runtime APIs (server credentials, player registration, login
// and refresh) for that project. Already-issued player access tokens expire normally.
func (s *Server) updateProject(c *gin.Context) {
	var in struct {
		Name   *string `json:"name"`
		Slug   *string `json:"slug"`
		Status *string `json:"status"`
	}
	if !bind(c, &in) {
		return
	}
	p := getPrincipal(c)
	if !ownerProject(c, s.db, c.Param("projectId")) {
		notFound(c)
		return
	}
	name, slug, status := "", "", ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 100 {
			failure(c, 422, "VALIDATION_ERROR", "Invalid project name.")
			return
		}
	}
	if in.Slug != nil {
		slug = strings.ToLower(strings.TrimSpace(*in.Slug))
		if !validSlug(slug) {
			failure(c, 422, "VALIDATION_ERROR", "Invalid project slug.")
			return
		}
	}
	if in.Status != nil {
		status = *in.Status
		if status != "active" && status != "archived" {
			failure(c, 422, "VALIDATION_ERROR", "Status must be active or archived.")
			return
		}
	}
	_, err := s.db.Exec(c, `UPDATE projects SET name=COALESCE(NULLIF($1,''),name),slug=COALESCE(NULLIF($2,''),slug),
		archived_at=CASE WHEN $3='archived' AND status<>'archived' THEN now() WHEN $3='active' THEN NULL ELSE archived_at END,
		status=COALESCE(NULLIF($3,''),status)
		WHERE id=$4 AND owner_user_id=$5`, name, slug, status, c.Param("projectId"), p.ID)
	if err != nil {
		if isUniqueViolation(err) {
			failure(c, 409, "CONFLICT", "Project slug is already in use.")
			return
		}
		internal(c, err)
		return
	}
	s.getProject(c)
}
func (s *Server) listEnvironments(c *gin.Context) {
	if !ownerProject(c, s.db, c.Param("projectId")) {
		notFound(c)
		return
	}
	rows, err := s.db.Query(c, `SELECT id::text,name,public_id,created_at FROM environments WHERE project_id=$1 ORDER BY name`, c.Param("projectId"))
	if err != nil {
		internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var id, n, pub string
		var at time.Time
		if err = rows.Scan(&id, &n, &pub, &at); err != nil {
			internal(c, err)
			return
		}
		out = append(out, gin.H{"id": id, "name": n, "public_id": pub, "created_at": at})
	}
	if err = rows.Err(); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, out)
}
func (s *Server) createEnvironment(c *gin.Context) {
	var in struct {
		Name string `json:"name"`
	}
	if !bind(c, &in) {
		return
	}
	if in.Name != "development" && in.Name != "production" {
		failure(c, 422, "VALIDATION_ERROR", "Environment must be development or production.")
		return
	}
	projectID := c.Param("projectId")
	if !ownerProject(c, s.db, projectID) {
		notFound(c)
		return
	}
	pub := "pub_" + randomSecret(18)
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var id string
	err = tx.QueryRow(c, `INSERT INTO environments(project_id,name,public_id) VALUES($1,$2,$3) RETURNING id::text`, projectID, in.Name, pub).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			failure(c, 409, "CONFLICT", "That environment already exists.")
			return
		}
		internal(c, err)
		return
	}
	if _, err = tx.Exec(c, `INSERT INTO configuration_drafts(project_id,environment_id) VALUES($1,$2)`, projectID, id); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, 201, gin.H{"id": id, "project_id": projectID, "name": in.Name, "public_id": pub})
}
func (s *Server) issueCredential(c *gin.Context) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	raw := randomSecret(36)
	prefix := "rs_" + randomID()[:12]
	var id string
	err := s.db.QueryRow(c, `INSERT INTO server_credentials(project_id,environment_id,key_prefix,secret_hash) VALUES($1,$2,$3,$4) RETURNING id::text`, projectID, envID, prefix, digest(raw)).Scan(&id)
	if err != nil {
		internal(c, err)
		return
	}
	success(c, 201, gin.H{"id": id, "credential": prefix + "." + raw, "warning": "Store this credential securely; it is shown only once."})
}
func (s *Server) listCredentials(c *gin.Context) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	rows, err := s.db.Query(c, `SELECT id::text,key_prefix,created_at,revoked_at FROM server_credentials WHERE project_id=$1 AND environment_id=$2 ORDER BY created_at DESC`, projectID, envID)
	if err != nil {
		internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var id, prefix string
		var created time.Time
		var revoked *time.Time
		if err := rows.Scan(&id, &prefix, &created, &revoked); err != nil {
			internal(c, err)
			return
		}
		entry := gin.H{"id": id, "key_prefix": prefix, "created_at": created}
		if revoked != nil {
			entry["revoked_at"] = revoked
		}
		out = append(out, entry)
	}
	if err = rows.Err(); err != nil {
		internal(c, err)
		return
	}
	success(c, 200, out)
}
func (s *Server) revokeCredential(c *gin.Context) {
	p := getPrincipal(c)
	tag, err := s.db.Exec(c, `UPDATE server_credentials sc SET revoked_at=now() FROM projects pr WHERE sc.id=$1 AND sc.project_id=pr.id AND pr.owner_user_id=$2 AND sc.revoked_at IS NULL`, c.Param("id"), p.ID)
	if err != nil {
		internal(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		notFound(c)
		return
	}
	success(c, 200, gin.H{"revoked": true})
}

var requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
