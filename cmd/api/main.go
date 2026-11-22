package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"realmstack-backend/internal/ai"
	"realmstack-backend/internal/demo"
	"realmstack-backend/internal/httpapi"
	"realmstack-backend/internal/limits"
	"realmstack-backend/internal/maintenance"
	"realmstack-backend/internal/persistence"
	"realmstack-backend/internal/platform"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// DATABASE_URL serves requests and may point at PgBouncer in transaction mode. Keep
	// pgx's default (cached prepared statements) and set max_prepared_statements on
	// PgBouncer 1.21+, which tracks them per server connection. Do not use
	// default_query_exec_mode=exec or simple_protocol: those encode arguments by Go type
	// without asking the server, so a []byte JSON argument is sent as bytea and every
	// json/jsonb write fails. DATABASE_DIRECT_URL, when
	// set, reaches PostgreSQL directly: migrations hold a session advisory lock, which a
	// transaction-mode pooler cannot keep, and maintenance runs there too.
	pool := connect(ctx, logger, "DATABASE_URL", env("DATABASE_URL", "postgres://realmstack:realmstack@localhost:5432/realmstack?sslmode=disable"))
	defer pool.Close()
	direct := pool
	if url := os.Getenv("DATABASE_DIRECT_URL"); url != "" {
		direct = connect(ctx, logger, "DATABASE_DIRECT_URL", url)
		defer direct.Close()
	}
	var replica *pgxpool.Pool
	if url := os.Getenv("DATABASE_REPLICA_URL"); url != "" {
		replica = connect(ctx, logger, "DATABASE_REPLICA_URL", url)
		defer replica.Close()
	}
	if cost := httpapi.ParseIntEnv("BCRYPT_COST", platform.BcryptCost); cost >= 4 && cost <= 31 {
		platform.BcryptCost = cost
	}
	var err error
	if err = persistence.Migrate(ctx, direct); err != nil {
		logger.Error("database migration failed", "error", err)
		os.Exit(1)
	}
	cfg := httpapi.Config{
		Env: env("APP_ENV", "production"), JWTSecret: os.Getenv("JWT_SECRET"),
		AccessTTL:      duration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTTL:     duration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		BodyLimit:      int64(httpapi.ParseIntEnv("BODY_LIMIT_BYTES", 1<<20)),
		DemoMode:       os.Getenv("DEMO_MODE") == "true",
		TrustedProxies: list(os.Getenv("TRUSTED_PROXIES")),
		AuthRateLimit:  httpapi.ParseIntEnv("AUTH_RATE_LIMIT_PER_MINUTE", 30),
		Quotas: limits.Limits{
			RequestsPerMinute: int64(httpapi.ParseIntEnv("QUOTA_REQUESTS_PER_MINUTE", 0)),
			WritesPerMinute:   int64(httpapi.ParseIntEnv("QUOTA_WRITES_PER_MINUTE", 0)),
			MaxBatchEntries:   httpapi.ParseIntEnv("QUOTA_MAX_BATCH_ENTRIES", 0),
		},
		CredentialCacheTTL: duration("CREDENTIAL_CACHE_TTL", 30*time.Second),
		BanSyncInterval:    duration("BAN_SYNC_INTERVAL", 5*time.Second),
		AI: ai.Config{
			EncryptionKey:   os.Getenv("AI_ENCRYPTION_KEY"),
			Timeout:         duration("AI_TIMEOUT", 3*time.Minute),
			MaxOutputTokens: int64(httpapi.ParseIntEnv("AI_MAX_OUTPUT_TOKENS", 32000)),
			PerMinute:       httpapi.ParseIntEnv("AI_GENERATIONS_PER_MINUTE", 6),
			MaxRunning:      httpapi.ParseIntEnv("AI_MAX_RUNNING_PER_USER", 2),
			// Off by default: users choose OpenAI-compatible base URLs (SSRF).
			AllowPrivateURLs: os.Getenv("AI_ALLOW_PRIVATE_URLS") == "true",
		},
	}
	// AI is optional: without a key the assistant is off and everything else works.
	// A key that is set but malformed is an operator mistake, so refuse to start.
	if cfg.AI.EncryptionKey != "" {
		if _, err = ai.NewSealer(cfg.AI.EncryptionKey); err != nil {
			logger.Error("invalid AI_ENCRYPTION_KEY", "error", err)
			os.Exit(1)
		}
	} else {
		logger.Info("AI assistant disabled (AI_ENCRYPTION_KEY not set)")
	}
	if err = ai.RecoverAbandoned(ctx, pool); err != nil {
		logger.Error("could not recover abandoned AI generations", "error", err)
		os.Exit(1)
	}
	if len(cfg.JWTSecret) < 32 {
		logger.Error("JWT_SECRET must be set to at least 32 characters")
		os.Exit(1)
	}
	if cfg.DemoMode && cfg.Env == "local" {
		if err = demo.Seed(ctx, pool, logger); err != nil {
			logger.Error("local demo seed failed", "error", err)
			os.Exit(1)
		}
	}
	api := httpapi.New(pool, cfg, logger)
	api.Env.ReadDB = replica // nil without a replica: reads use the primary
	// REDIS_URL enables the leaderboard rank projection. Redis is optional and never
	// the source of truth: when it is unreachable, ranks fall back to PostgreSQL and
	// the outbox keeps the changes until it returns, so it does not block startup.
	if url := os.Getenv("REDIS_URL"); url != "" {
		opts, err := redis.ParseURL(url)
		if err != nil {
			logger.Error("invalid REDIS_URL", "error", err)
			os.Exit(1)
		}
		rdb := redis.NewClient(opts)
		defer rdb.Close()
		if err = rdb.Ping(ctx).Err(); err != nil {
			logger.Warn("redis unreachable at startup; ranks use PostgreSQL until it is", "error", err)
		}
		api.Env.Redis = rdb
	}
	srv := api.HTTPServer(env("HTTP_ADDR", ":8080"))
	background, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	api.Start(background)
	go maintenance.Run(background, direct, logger, duration("MAINTENANCE_INTERVAL", time.Hour))

	// Stop accepting connections on SIGINT/SIGTERM and let in-flight requests finish.
	stop, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()
	select {
	case err = <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
			os.Exit(1)
		}
	case <-stop.Done():
		logger.Info("shutting down")
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelShutdown()
		if err = srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
		// Let running AI generations finish within the grace period. Any still running
		// when the process exits are marked failed by RecoverAbandoned on the next start.
		done := make(chan struct{})
		go func() { api.AI.Wait(); close(done) }()
		select {
		case <-done:
		case <-shutdownCtx.Done():
			logger.Warn("AI generations still running at shutdown; they will be marked failed on restart")
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// connect opens a pool and waits up to the startup deadline for the database.
func connect(ctx context.Context, logger *slog.Logger, name, url string) *pgxpool.Pool {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		logger.Error("database configuration failed", "setting", name, "error", err)
		os.Exit(1)
	}
	for {
		if err = pool.Ping(ctx); err == nil {
			return pool
		}
		select {
		case <-ctx.Done():
			logger.Error("database connection failed", "setting", name, "error", err)
			os.Exit(1)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func duration(key string, fallback time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return fallback
}

// list splits a comma-separated environment value, dropping empty entries.
func list(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
