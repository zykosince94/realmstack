// Package players owns player identity: registration and login selected by a public
// environment identifier, rotating refresh tokens with reuse detection, and the profile.
package players

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
	"realmstack-backend/internal/platform"
)

// Handler serves player identity routes.
type Handler struct{ *platform.Env }

// LookupPublic resolves a public environment identifier of an active project. It
// accepts only the public identifier, never the internal environment UUID, and
// answers 404 itself when the lookup fails.
func LookupPublic(c *gin.Context, q platform.Querier, public string) (platform.Principal, bool) {
	var p platform.Principal
	err := q.QueryRow(c, `SELECT e.project_id::text,e.id::text FROM environments e JOIN projects pr ON pr.id=e.project_id WHERE e.public_id=$1 AND pr.status='active'`, public).Scan(&p.ProjectID, &p.EnvironmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		platform.Fail(c, http.StatusNotFound, "NOT_FOUND", "The public project environment was not found.")
		return p, false
	}
	if err != nil {
		platform.Internal(c, err)
		return p, false
	}
	return p, true
}

func (h Handler) resolve(c *gin.Context, public, legacy string) (platform.Principal, bool) {
	if public == "" {
		public = legacy
	}
	if public == "" {
		public = c.GetHeader("X-Public-Environment")
	}
	return LookupPublic(c, h.DB, public)
}

// defaultDisplayName never derives from the email address, because display names are
// public on leaderboards.
func defaultDisplayName() string { return "Player-" + strings.ToUpper(platform.RandomID()[:6]) }

func (h Handler) issueTokens(c *gin.Context, id string, scope platform.Principal) (string, string, error) {
	raw := platform.RandomSecret(48)
	_, err := h.DB.Exec(c, `INSERT INTO refresh_tokens(player_id,family_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`, id, platform.RandomUUID(), platform.Digest(raw), h.Now().Add(h.RefreshTTL))
	if err != nil {
		return "", "", err
	}
	return h.Token(platform.Principal{ID: id, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Kind: platform.KindPlayer}, h.AccessTTL), raw, nil
}

func (h Handler) session(player gin.H, access, refresh string) gin.H {
	return gin.H{"player": player, "access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": int(h.AccessTTL.Seconds())}
}

// Register creates a player in the selected environment. external_player_key is
// reserved for trusted-server linking and cannot be set here, so nobody can claim
// another person's external identity.
func (h Handler) Register(c *gin.Context) {
	var in struct {
		PublicEnvironment string `json:"public_environment_id"`
		EnvironmentID     string `json:"environment_id"`
		Email             string `json:"email"`
		Password          string `json:"password"`
		DisplayName       string `json:"display_name"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	scope, ok := h.resolve(c, in.PublicEnvironment, in.EnvironmentID)
	if !ok {
		return
	}
	email := platform.NormalizeEmail(in.Email)
	if len(email) < 5 || len(email) > 254 || len(in.Password) < 8 || len(in.Password) > 128 {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Email or password is invalid.")
		return
	}
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = defaultDisplayName()
	}
	if len(display) > 80 {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Display name is too long.")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	var id string
	err = h.DB.QueryRow(c, `INSERT INTO players(project_id,environment_id,email,password_hash,display_name) VALUES($1,$2,$3,$4,$5) RETURNING id::text`,
		scope.ProjectID, scope.EnvironmentID, email, string(hash), display).Scan(&id)
	if platform.IsUniqueViolation(err) {
		platform.Fail(c, http.StatusConflict, "CONFLICT", "A player with that email already exists.")
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	access, refresh, err := h.issueTokens(c, id, scope)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusCreated, h.session(gin.H{"id": id, "email": email, "display_name": display}, access, refresh))
}

// Login signs a player in to the selected environment.
func (h Handler) Login(c *gin.Context) {
	var in struct {
		PublicEnvironment string `json:"public_environment_id"`
		EnvironmentID     string `json:"environment_id"`
		Email             string `json:"email"`
		Password          string `json:"password"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	scope, ok := h.resolve(c, in.PublicEnvironment, in.EnvironmentID)
	if !ok {
		return
	}
	var id, hash, display string
	err := h.DB.QueryRow(c, `SELECT id::text,COALESCE(password_hash,''),display_name FROM players WHERE project_id=$1 AND environment_id=$2 AND email=$3`,
		scope.ProjectID, scope.EnvironmentID, platform.NormalizeEmail(in.Email)).Scan(&id, &hash, &display)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		platform.Internal(c, err)
		return
	}
	if !platform.CheckPassword(err == nil && hash != "", hash, in.Password) {
		platform.Fail(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is incorrect.")
		return
	}
	access, refresh, err := h.issueTokens(c, id, scope)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, h.session(gin.H{"id": id, "display_name": display}, access, refresh))
}

// Refresh rotates a refresh token. Presenting an already-rotated token revokes its
// whole family, because it means the token was copied.
func (h Handler) Refresh(c *gin.Context) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	if len(in.RefreshToken) < 20 {
		platform.Fail(c, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "Refresh token is invalid.")
		return
	}
	tx, err := h.DB.Begin(c)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var tokenID, playerID, family string
	var scope platform.Principal
	var revoked *time.Time
	var expires time.Time
	err = tx.QueryRow(c, `SELECT rt.id::text,rt.player_id::text,rt.family_id,rt.revoked_at,rt.expires_at,p.project_id::text,p.environment_id::text
		FROM refresh_tokens rt JOIN players p ON p.id=rt.player_id JOIN projects pr ON pr.id=p.project_id AND pr.status='active'
		WHERE rt.token_hash=$1 FOR UPDATE OF rt`, platform.Digest(in.RefreshToken)).Scan(&tokenID, &playerID, &family, &revoked, &expires, &scope.ProjectID, &scope.EnvironmentID)
	if err != nil {
		platform.Fail(c, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "Refresh token is invalid or expired.")
		return
	}
	if revoked != nil {
		_, _ = tx.Exec(c, `UPDATE refresh_tokens SET revoked_at=COALESCE(revoked_at,now()) WHERE family_id=$1`, family)
		_ = tx.Commit(c)
		platform.Fail(c, http.StatusUnauthorized, "REFRESH_TOKEN_REUSE", "Refresh token reuse detected; the token family was revoked.")
		return
	}
	if !h.Now().Before(expires) {
		platform.Fail(c, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "Refresh token is expired.")
		return
	}
	raw := platform.RandomSecret(48)
	var newID string
	if err = tx.QueryRow(c, `INSERT INTO refresh_tokens(player_id,family_id,token_hash,expires_at) VALUES($1,$2,$3,$4) RETURNING id::text`,
		playerID, family, platform.Digest(raw), h.Now().Add(h.RefreshTTL)).Scan(&newID); err != nil {
		platform.Internal(c, err)
		return
	}
	if _, err = tx.Exec(c, `UPDATE refresh_tokens SET revoked_at=now(),replaced_by=$1 WHERE id=$2`, newID, tokenID); err != nil {
		platform.Internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		platform.Internal(c, err)
		return
	}
	access := h.Token(platform.Principal{ID: playerID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Kind: platform.KindPlayer}, h.AccessTTL)
	platform.Success(c, http.StatusOK, gin.H{"access_token": access, "refresh_token": raw, "token_type": "Bearer", "expires_in": int(h.AccessTTL.Seconds())})
}

func profile(c *gin.Context, q platform.Querier, p platform.Principal) (gin.H, error) {
	var email, display string
	var metadata []byte
	err := q.QueryRow(c, `SELECT COALESCE(email,''),display_name,metadata FROM players WHERE id=$1 AND project_id=$2 AND environment_id=$3`, p.ID, p.ProjectID, p.EnvironmentID).Scan(&email, &display, &metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platform.Errf(http.StatusNotFound, "NOT_FOUND", "The requested resource was not found.")
	}
	return gin.H{"id": p.ID, "email": email, "display_name": display, "metadata": json.RawMessage(metadata)}, err
}

// Me returns the player's profile.
func (h Handler) Me(c *gin.Context) {
	out, err := profile(c, h.DB, platform.GetPrincipal(c))
	if err != nil {
		platform.Respond(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}

// PatchMe updates the display name and/or replaces metadata (max 16 KiB).
func (h Handler) PatchMe(c *gin.Context) {
	var in struct {
		DisplayName *string        `json:"display_name"`
		Metadata    map[string]any `json:"metadata"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	if in.DisplayName == nil && in.Metadata == nil {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Provide display_name or metadata.")
		return
	}
	if in.DisplayName != nil && (strings.TrimSpace(*in.DisplayName) == "" || len(*in.DisplayName) > 80) {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Display name is invalid.")
		return
	}
	meta := []byte("null")
	if in.Metadata != nil {
		var err error
		if meta, err = json.Marshal(in.Metadata); err != nil || len(meta) > 16384 {
			platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Metadata is invalid or too large.")
			return
		}
	}
	p := platform.GetPrincipal(c)
	h.Mutate(c, p, in, func(tx pgx.Tx) (int, any, error) {
		_, err := tx.Exec(c, `UPDATE players SET display_name=COALESCE($1,display_name),metadata=CASE WHEN $2::jsonb='null'::jsonb THEN metadata ELSE $2::jsonb END
			WHERE id=$3 AND project_id=$4 AND environment_id=$5`, in.DisplayName, meta, p.ID, p.ProjectID, p.EnvironmentID)
		if err != nil {
			return 0, nil, err
		}
		out, err := profile(c, tx, p)
		return http.StatusOK, out, err
	})
}
