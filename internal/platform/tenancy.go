package platform

import (
	"errors"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// SetOwned stores the project/environment that ownership middleware verified the
// dashboard user owns.
func SetOwned(c *gin.Context, projectID, envID string) {
	c.Set("owned_project", projectID)
	c.Set("owned_environment", envID)
}

// Owned returns the verified project and environment for a dashboard route.
func Owned(c *gin.Context) (projectID, envID string) {
	return c.GetString("owned_project"), c.GetString("owned_environment")
}

// SetDashboardUser records which dashboard user is acting. playeradmin.Target calls it
// before it replaces the principal with the managed player, so routes that record who
// did something (bans) can still tell.
func SetDashboardUser(c *gin.Context, userID string) { c.Set("dashboard_user", userID) }

// DashboardUser returns the acting dashboard user's ID on a player management route.
func DashboardUser(c *gin.Context) string { return c.GetString("dashboard_user") }

// PlayerInScope reports whether playerID belongs to the scope's project/environment.
// A UUID is never authorization on its own: this predicate is.
//
// Positive answers are cached in memory: every server write checks scope, and
// the lookup was one PostgreSQL round trip per request. Caching only "yes" is safe
// because a player row never moves to another project or environment and is never
// deleted (projects are archived, and archiving is enforced on the credential, whose
// cache lives at most 60 s). If players ever become deletable, invalidate this cache.
func PlayerInScope(c *gin.Context, q Querier, scope Principal, playerID string) bool {
	cacheKey := scope.ProjectID + "/" + scope.EnvironmentID + "/" + playerID
	if playerScopes.has(cacheKey) {
		return true
	}
	var exists bool
	err := q.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM players WHERE id=$1 AND project_id=$2 AND environment_id=$3)`, playerID, scope.ProjectID, scope.EnvironmentID).Scan(&exists)
	if err == nil && exists {
		playerScopes.add(cacheKey)
	}
	return err == nil && exists
}

// scopeSet is a bounded set of confirmed player scopes. When it fills up it starts
// over: simpler than LRU, and a miss only costs the lookup it saves.
type scopeSet struct {
	mu  sync.RWMutex
	max int
	m   map[string]struct{}
}

var playerScopes = &scopeSet{max: 200_000, m: map[string]struct{}{}}

func (s *scopeSet) has(k string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.m[k]
	return ok
}

func (s *scopeSet) add(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= s.max {
		s.m = make(map[string]struct{}, s.max)
	}
	s.m[k] = struct{}{}
}

// ServerTarget returns the server principal re-targeted at the :playerId route param,
// after checking the player is in the credential's scope. It answers 404 otherwise.
func ServerTarget(c *gin.Context, q Querier) (Principal, bool) {
	p := GetPrincipal(c)
	playerID := c.Param("playerId")
	if !PlayerInScope(c, q, p, playerID) {
		NotFound(c)
		return p, false
	}
	p.ID = playerID
	return p, true
}

// LockPlayer locks the player row for the rest of tx, serializing operations that
// read and then write several of a player's rows (claims, XP pinning).
func LockPlayer(c *gin.Context, tx pgx.Tx, scope Principal, playerID string) error {
	err := tx.QueryRow(c, `SELECT 1 FROM players WHERE id=$1 AND project_id=$2 AND environment_id=$3 FOR UPDATE`, playerID, scope.ProjectID, scope.EnvironmentID).Scan(new(int))
	if errors.Is(err, pgx.ErrNoRows) {
		return &Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "The requested resource was not found."}
	}
	return err
}
