package httpapi

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"realmstack-backend/internal/limits"
	"realmstack-backend/internal/platform"
)

// rateLimiter is a fixed-window counter per key. It is per process: with several API
// instances each enforces its own budget (a shared store is planned for Phase 8).
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]limitEntry
}

type limitEntry struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, entries: map[string]limitEntry{}}
}

// allow records one attempt for key and reports whether it is within the limit,
// plus the time until the current window resets.
func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if !now.Before(entry.reset) {
		entry = limitEntry{reset: now.Add(l.window)}
	}
	entry.count++
	l.entries[key] = entry
	if len(l.entries) > 10000 {
		for k, v := range l.entries {
			if !now.Before(v.reset) {
				delete(l.entries, k)
			}
		}
	}
	return entry.count <= l.limit, entry.reset.Sub(now)
}

// rateLimit limits authentication attempts per client IP and route. With a database
// the count is shared by every instance (limits.Exact); the in-memory limiter remains
// only for unit tests that run without one.
func (s *Server) rateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		// ClientIP honours X-Forwarded-For only from Config.TrustedProxies.
		bucket := "auth:" + c.ClientIP() + "|" + c.FullPath()
		var allowed bool
		var retry time.Duration
		if s.Env.DB != nil {
			var err error
			if allowed, retry, err = limits.Exact(c, s.Env.DB, bucket, s.cfg.AuthRateLimit, time.Minute, s.Env.Now()); err != nil {
				s.Env.Log.Error("rate limit check failed; allowing the request", "error", err)
			}
		} else {
			allowed, retry = s.limiter.allow(bucket, time.Now())
		}
		if !allowed {
			c.Header("Retry-After", limits.RetryAfterHeader(retry))
			platform.Fail(c, http.StatusTooManyRequests, "RATE_LIMITED", "Too many authentication attempts; try again shortly.")
			return
		}
		c.Next()
	}
}

// quota enforces the tenant's request and write budgets (project_quotas) on runtime
// routes, after authentication has established the project. Batch requests count
// their entries as writes in the batch handler instead of one write here.
func (s *Server) quota() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.Quotas == nil {
			c.Next()
			return
		}
		projectID := platform.GetScope(c).ProjectID
		if platform.HasPrincipal(c) {
			projectID = platform.GetPrincipal(c).ProjectID
		}
		ok, retry := s.Quotas.Take(c, projectID, limits.Requests, 1)
		if ok && c.Request.Method != http.MethodGet && c.FullPath() != "/v1/server/batch" {
			ok, retry = s.Quotas.Take(c, projectID, limits.Writes, 1)
		}
		if !ok {
			c.Header("Retry-After", limits.RetryAfterHeader(retry))
			platform.Fail(c, http.StatusTooManyRequests, "QUOTA_EXCEEDED", "The project's request quota is exhausted; retry after the indicated time.")
			return
		}
		c.Next()
	}
}
