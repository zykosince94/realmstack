// Package projects manages tenants: projects, their environments, and the
// environment-scoped server credentials. It also provides the ownership middleware
// every dashboard route behind /projects/:projectId uses.
package projects

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// Handler serves project, environment and credential routes.
type Handler struct{ *platform.Env }

func (h Handler) ownsProject(c *gin.Context, projectID string) bool {
	var exists bool
	err := h.DB.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1 AND owner_user_id=$2)`, projectID, platform.GetPrincipal(c).ID).Scan(&exists)
	return err == nil && exists
}

// RequireOwnedProject answers 404 unless the dashboard user owns :projectId.
func (h Handler) RequireOwnedProject() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !h.ownsProject(c, c.Param("projectId")) {
			platform.NotFound(c)
			return
		}
		platform.SetOwned(c, c.Param("projectId"), "")
		c.Next()
	}
}

// RequireOwnedEnvironment answers 404 unless the user owns :projectId and :envId
// belongs to it; handlers then read the verified scope with platform.Owned.
func (h Handler) RequireOwnedEnvironment() gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, envID := c.Param("projectId"), c.Param("envId")
		var exists bool
		err := h.DB.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1 AND e.project_id=$2 AND p.owner_user_id=$3)`,
			envID, projectID, platform.GetPrincipal(c).ID).Scan(&exists)
		if err != nil || !exists {
			platform.NotFound(c)
			return
		}
		platform.SetOwned(c, projectID, envID)
		c.Next()
	}
}

func projectView(id, name, slug, status string, created time.Time, archived *time.Time) gin.H {
	out := gin.H{"id": id, "name": name, "slug": slug, "status": status, "created_at": created}
	if archived != nil {
		out["archived_at"] = archived
	}
	return out
}

// List lists the caller's active projects; ?include_archived=true adds archived ones.
func (h Handler) List(c *gin.Context) {
	rows, err := h.DB.Query(c, `SELECT id::text,name,slug,status,created_at,archived_at FROM projects WHERE owner_user_id=$1 AND ($2 OR status='active') ORDER BY created_at`,
		platform.GetPrincipal(c).ID, c.Query("include_archived") == "true")
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var id, name, slug, status string
		var created time.Time
		var archived *time.Time
		if err = rows.Scan(&id, &name, &slug, &status, &created, &archived); err != nil {
			platform.Internal(c, err)
			return
		}
		out = append(out, projectView(id, name, slug, status, created, archived))
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}

// createEnvironmentTx inserts an environment and its empty configuration draft.
func createEnvironmentTx(c *gin.Context, tx pgx.Tx, projectID, name string) (id, public string, err error) {
	public = "pub_" + platform.RandomSecret(18)
	if err = tx.QueryRow(c, `INSERT INTO environments(project_id,name,public_id) VALUES($1,$2,$3) RETURNING id::text`, projectID, name, public).Scan(&id); err != nil {
		return "", "", err
	}
	_, err = tx.Exec(c, `INSERT INTO configuration_drafts(project_id,environment_id) VALUES($1,$2)`, projectID, id)
	return id, public, err
}

// Create creates a project with a development environment.
func (h Handler) Create(c *gin.Context) {
	var in struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) < 1 || len(in.Name) > 100 || !platform.ValidSlug(in.Slug) {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "A project name and valid slug are required.")
		return
	}
	tx, err := h.DB.Begin(c)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var id string
	err = tx.QueryRow(c, `INSERT INTO projects(owner_user_id,name,slug) VALUES($1,$2,$3) RETURNING id::text`, platform.GetPrincipal(c).ID, in.Name, in.Slug).Scan(&id)
	if platform.IsUniqueViolation(err) {
		platform.Fail(c, http.StatusConflict, "CONFLICT", "Project slug is already in use.")
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	envID, public, err := createEnvironmentTx(c, tx, id, "development")
	if err != nil {
		platform.Internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusCreated, gin.H{"id": id, "name": in.Name, "slug": in.Slug, "status": "active", "environments": []gin.H{{"id": envID, "name": "development", "public_id": public}}})
}

// Get returns one owned project.
func (h Handler) Get(c *gin.Context) {
	var id, name, slug, status string
	var created time.Time
	var archived *time.Time
	err := h.DB.QueryRow(c, `SELECT id::text,name,slug,status,created_at,archived_at FROM projects WHERE id=$1 AND owner_user_id=$2`, c.Param("projectId"), platform.GetPrincipal(c).ID).
		Scan(&id, &name, &slug, &status, &created, &archived)
	if errors.Is(err, pgx.ErrNoRows) {
		platform.NotFound(c)
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, projectView(id, name, slug, status, created, archived))
}

// Update renames a project, changes its slug, or archives/restores it with
// {"status":"archived"|"active"}. Projects are never deleted: archiving keeps every
// ledger and stops the runtime APIs (server credentials, player registration, login,
// refresh and public reads). Already-issued player access tokens expire normally.
func (h Handler) Update(c *gin.Context) {
	var in struct {
		Name   *string `json:"name"`
		Slug   *string `json:"slug"`
		Status *string `json:"status"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	name, slug, status := "", "", ""
	if in.Name != nil {
		if name = strings.TrimSpace(*in.Name); name == "" || len(name) > 100 {
			platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Invalid project name.")
			return
		}
	}
	if in.Slug != nil {
		if slug = strings.ToLower(strings.TrimSpace(*in.Slug)); !platform.ValidSlug(slug) {
			platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Invalid project slug.")
			return
		}
	}
	if in.Status != nil {
		if status = *in.Status; status != "active" && status != "archived" {
			platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Status must be active or archived.")
			return
		}
	}
	_, err := h.DB.Exec(c, `UPDATE projects SET name=COALESCE(NULLIF($1,''),name),slug=COALESCE(NULLIF($2,''),slug),
		archived_at=CASE WHEN $3='archived' AND status<>'archived' THEN now() WHEN $3='active' THEN NULL ELSE archived_at END,
		status=COALESCE(NULLIF($3,''),status)
		WHERE id=$4 AND owner_user_id=$5`, name, slug, status, c.Param("projectId"), platform.GetPrincipal(c).ID)
	if platform.IsUniqueViolation(err) {
		platform.Fail(c, http.StatusConflict, "CONFLICT", "Project slug is already in use.")
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	if status == "archived" {
		h.Credentials.ForgetProject(c.Param("projectId")) // its server credentials stop now on this instance
	}
	h.Get(c)
}

// ListEnvironments lists an owned project's environments.
func (h Handler) ListEnvironments(c *gin.Context) {
	rows, err := h.DB.Query(c, `SELECT id::text,name,public_id,created_at FROM environments WHERE project_id=$1 ORDER BY name`, c.Param("projectId"))
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var id, name, public string
		var at time.Time
		if err = rows.Scan(&id, &name, &public, &at); err != nil {
			platform.Internal(c, err)
			return
		}
		out = append(out, gin.H{"id": id, "name": name, "public_id": public, "created_at": at})
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}

// CreateEnvironment adds the development or production environment to a project.
func (h Handler) CreateEnvironment(c *gin.Context) {
	var in struct {
		Name string `json:"name"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	if in.Name != "development" && in.Name != "production" {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Environment must be development or production.")
		return
	}
	projectID := c.Param("projectId")
	tx, err := h.DB.Begin(c)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer tx.Rollback(c)
	id, public, err := createEnvironmentTx(c, tx, projectID, in.Name)
	if platform.IsUniqueViolation(err) {
		platform.Fail(c, http.StatusConflict, "CONFLICT", "That environment already exists.")
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusCreated, gin.H{"id": id, "project_id": projectID, "name": in.Name, "public_id": public})
}

// IssueCredential creates an environment-scoped server credential; the secret is
// returned only in this response and stored only as a digest.
func (h Handler) IssueCredential(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	raw := platform.RandomSecret(36)
	prefix := "rs_" + platform.RandomID()[:12]
	var id string
	err := h.DB.QueryRow(c, `INSERT INTO server_credentials(project_id,environment_id,key_prefix,secret_hash) VALUES($1,$2,$3,$4) RETURNING id::text`, projectID, envID, prefix, platform.Digest(raw)).Scan(&id)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusCreated, gin.H{"id": id, "credential": prefix + "." + raw, "warning": "Store this credential securely; it is shown only once."})
}

// ListCredentials lists credential metadata (never secrets) for an environment.
func (h Handler) ListCredentials(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	rows, err := h.DB.Query(c, `SELECT id::text,key_prefix,created_at,revoked_at FROM server_credentials WHERE project_id=$1 AND environment_id=$2 ORDER BY created_at DESC`, projectID, envID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var id, prefix string
		var created time.Time
		var revoked *time.Time
		if err = rows.Scan(&id, &prefix, &created, &revoked); err != nil {
			platform.Internal(c, err)
			return
		}
		entry := gin.H{"id": id, "key_prefix": prefix, "created_at": created}
		if revoked != nil {
			entry["revoked_at"] = revoked
		}
		out = append(out, entry)
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}

// RevokeCredential revokes a credential of a project the caller owns.
func (h Handler) RevokeCredential(c *gin.Context) {
	var prefix string
	err := h.DB.QueryRow(c, `UPDATE server_credentials sc SET revoked_at=now() FROM projects pr WHERE sc.id=$1 AND sc.project_id=pr.id AND pr.owner_user_id=$2 AND sc.revoked_at IS NULL RETURNING sc.key_prefix`,
		c.Param("id"), platform.GetPrincipal(c).ID).Scan(&prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		platform.NotFound(c)
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	// Stop it working on this instance now; other instances drop it within the
	// credential cache's TTL (at most 60 seconds).
	h.Credentials.Forget(prefix)
	platform.Success(c, http.StatusOK, gin.H{"revoked": true})
}
