package httpapi

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
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

func (s *Server) rateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		// ClientIP honours X-Forwarded-For only from Config.TrustedProxies.
		allowed, retry := s.limiter.allow(c.ClientIP()+"|"+c.FullPath(), time.Now())
		if !allowed {
			c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			platform.Fail(c, http.StatusTooManyRequests, "RATE_LIMITED", "Too many authentication attempts; try again shortly.")
			return
		}
		c.Next()
	}
}
