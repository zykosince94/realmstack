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
	"realmstack-backend/internal/ai"
	"realmstack-backend/internal/demo"
	"realmstack-backend/internal/httpapi"
	"realmstack-backend/internal/persistence"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://realmstack:realmstack@localhost:5432/realmstack?sslmode=disable"))
	if err != nil {
		logger.Error("database configuration failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	for {
		err = pool.Ping(ctx)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			logger.Error("database connection failed", "error", err)
			os.Exit(1)
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err = persistence.Migrate(ctx, pool); err != nil {
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
		AI: ai.Config{
			EncryptionKey:   os.Getenv("AI_ENCRYPTION_KEY"),
			Timeout:         duration("AI_TIMEOUT", 3*time.Minute),
			MaxOutputTokens: int64(httpapi.ParseIntEnv("AI_MAX_OUTPUT_TOKENS", 32000)),
			PerMinute:       httpapi.ParseIntEnv("AI_GENERATIONS_PER_MINUTE", 6),
			MaxRunning:      httpapi.ParseIntEnv("AI_MAX_RUNNING_PER_USER", 2),
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
	srv := api.HTTPServer(env("HTTP_ADDR", ":8080"))

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
