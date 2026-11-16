package platform

import (
	"errors"
	"net/http"

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

// PlayerInScope reports whether playerID belongs to the scope's project/environment.
// A UUID is never authorization on its own: this predicate is.
func PlayerInScope(c *gin.Context, q Querier, scope Principal, playerID string) bool {
	var exists bool
	err := q.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM players WHERE id=$1 AND project_id=$2 AND environment_id=$3)`, playerID, scope.ProjectID, scope.EnvironmentID).Scan(&exists)
	return err == nil && exists
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
