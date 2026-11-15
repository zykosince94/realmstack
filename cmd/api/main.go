package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"realmstack-backend/internal/app"
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
	if err = app.Migrate(ctx, pool); err != nil {
		logger.Error("database migration failed", "error", err)
		os.Exit(1)
	}
	cfg := app.Config{
		Env: env("APP_ENV", "production"), JWTSecret: os.Getenv("JWT_SECRET"),
		AccessTTL:  duration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTTL: duration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		BodyLimit:  int64(app.ParseIntEnv("BODY_LIMIT_BYTES", 1<<20)),
		DemoMode:   os.Getenv("DEMO_MODE") == "true",
	}
	if len(cfg.JWTSecret) < 32 {
		logger.Error("JWT_SECRET must be set to at least 32 characters")
		os.Exit(1)
	}
	if cfg.DemoMode && cfg.Env == "local" {
		if err = app.SeedDemo(ctx, pool, logger); err != nil {
			logger.Error("local demo seed failed", "error", err)
			os.Exit(1)
		}
	}
	server := app.New(pool, cfg, logger)
	if err := server.Run(env("HTTP_ADDR", ":8080")); err != nil {
		logger.Error("http server stopped", "error", err)
		os.Exit(1)
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
