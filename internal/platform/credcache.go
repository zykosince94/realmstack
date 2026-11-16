package platform

import (
	"sync"
	"time"
)

// CredentialCache keeps successful server-credential lookups for a short time (spec
// 6.6: at most 60 seconds), so a game server sending thousands of writes per second
// does not cost one credential query per request. The secret is still checked against
// the cached digest on every request; only the database lookup is skipped.
//
// Revoking a credential, or archiving its project, drops its entries on this instance
// immediately. Other instances notice within the TTL, which is the documented bound on
// how long a revoked credential can keep working.
type CredentialCache struct {
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]CachedCredential
}

// CachedCredential is what a successful lookup returns.
type CachedCredential struct {
	ProjectID, EnvironmentID, SecretHash string
	expires                              time.Time
}

// NewCredentialCache returns a cache with the given TTL (capped at 60 seconds).
func NewCredentialCache(ttl time.Duration, now func() time.Time) *CredentialCache {
	if ttl > time.Minute {
		ttl = time.Minute
	}
	return &CredentialCache{ttl: ttl, now: now, entries: map[string]CachedCredential{}}
}

// Get returns a live entry for prefix.
func (c *CredentialCache) Get(prefix string) (CachedCredential, bool) {
	if c == nil || c.ttl <= 0 {
		return CachedCredential{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[prefix]
	if !ok || !c.now().Before(e.expires) {
		delete(c.entries, prefix)
		return CachedCredential{}, false
	}
	return e, true
}

// Put stores a successful lookup.
func (c *CredentialCache) Put(prefix string, e CachedCredential) {
	if c == nil || c.ttl <= 0 {
		return
	}
	e.expires = c.now().Add(c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[prefix] = e
}

// Forget drops one credential (on revocation).
func (c *CredentialCache) Forget(prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, prefix)
}

// ForgetProject drops every credential of a project (on archiving).
func (c *CredentialCache) ForgetProject(projectID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for prefix, e := range c.entries {
		if e.ProjectID == projectID {
			delete(c.entries, prefix)
		}
	}
}
