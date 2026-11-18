// Package auth registers and signs in dashboard users (game developers).
//
// A dashboard session is a short-lived access token (returned in the body, held in the
// page's memory) plus a rotating refresh token in an httpOnly cookie. Before sessions,
// the access token lived only in memory, so every page reload signed the user out. The
// cookie fixes that without letting page scripts read anything long-lived: JavaScript
// never sees the refresh token, SameSite=Strict keeps other sites from sending it, and
// its path limits it to /v1/auth. Reuse of a rotated token revokes the whole sign-in
// (token family), the same rule as player refresh tokens.
package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// RefreshCookie is the dashboard refresh-token cookie.
const RefreshCookie = "rs_dashboard_refresh"

// cookiePath limits the cookie to the auth routes: no other request carries it.
const cookiePath = "/v1/auth"

// Handler serves the dashboard authentication routes.
type Handler struct{ *platform.Env }

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// session issues an access token and starts a new refresh-token family.
func (h Handler) session(c *gin.Context, status int, id, email string) {
	raw := platform.RandomSecret(32)
	if _, err := h.DB.Exec(c, `INSERT INTO dashboard_refresh_tokens(user_id,family_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`,
		id, platform.RandomUUID(), platform.Digest(raw), h.Now().Add(h.RefreshTTL)); err != nil {
		platform.Internal(c, err)
		return
	}
	h.respond(c, status, id, email, raw)
}

func (h Handler) respond(c *gin.Context, status int, id, email, refresh string) {
	h.setCookie(c, refresh, h.RefreshTTL)
	platform.Success(c, status, gin.H{
		"user":         gin.H{"id": id, "email": email},
		"access_token": h.Token(platform.Principal{ID: id, Kind: platform.KindUser}, h.AccessTTL),
		"token_type":   "Bearer",
		"expires_in":   int(h.AccessTTL.Seconds()),
	})
}

// setCookie writes (or, with ttl 0, clears) the refresh cookie. Secure is off only for
// local development over plain http, where browsers would otherwise drop the cookie.
func (h Handler) setCookie(c *gin.Context, value string, ttl time.Duration) {
	maxAge := int(ttl.Seconds())
	if ttl <= 0 {
		value, maxAge = "", -1
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name: RefreshCookie, Value: value, Path: cookiePath, MaxAge: maxAge,
		HttpOnly: true, Secure: h.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
}

// Register creates a dashboard user and signs them in.
func (h Handler) Register(c *gin.Context) {
	var in credentials
	if !platform.Bind(c, &in) {
		return
	}
	in.Email = platform.NormalizeEmail(in.Email)
	if len(in.Email) < 5 || len(in.Email) > 254 || len(in.Password) < 10 || len(in.Password) > 128 {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Email or password does not meet requirements.")
		return
	}
	hash, err := platform.HashPassword(in.Password)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	var id string
	err = h.DB.QueryRow(c, `INSERT INTO users(email,password_hash) VALUES($1,$2) RETURNING id::text`, in.Email, string(hash)).Scan(&id)
	if platform.IsUniqueViolation(err) {
		platform.Fail(c, http.StatusConflict, "CONFLICT", "An account with that email already exists.")
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	h.session(c, http.StatusCreated, id, in.Email)
}

// Login signs in a dashboard user.
func (h Handler) Login(c *gin.Context) {
	var in credentials
	if !platform.Bind(c, &in) {
		return
	}
	email := platform.NormalizeEmail(in.Email)
	var id, hash string
	err := h.DB.QueryRow(c, `SELECT id::text,password_hash FROM users WHERE email=$1`, email).Scan(&id, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		platform.Internal(c, err)
		return
	}
	if !platform.CheckPassword(err == nil, hash, in.Password) {
		platform.Fail(c, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Email or password is incorrect.")
		return
	}
	h.session(c, http.StatusOK, id, email)
}

// Refresh exchanges the refresh cookie for a new access token and rotates the cookie.
// The dashboard calls it on load (so a reload keeps the session) and shortly before the
// access token expires.
func (h Handler) Refresh(c *gin.Context) {
	raw, err := c.Cookie(RefreshCookie)
	if err != nil || raw == "" {
		platform.Fail(c, http.StatusUnauthorized, "NO_SESSION", "There is no dashboard session to resume. Sign in.")
		return
	}
	tx, err := h.DB.Begin(c)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var tokenID, userID, family, email string
	var revoked *time.Time
	var expires time.Time
	err = tx.QueryRow(c, `SELECT rt.id::text,rt.user_id::text,rt.family_id::text,rt.revoked_at,rt.expires_at,u.email
		FROM dashboard_refresh_tokens rt JOIN users u ON u.id=rt.user_id WHERE rt.token_hash=$1 FOR UPDATE OF rt`, platform.Digest(raw)).
		Scan(&tokenID, &userID, &family, &revoked, &expires, &email)
	if err != nil {
		h.setCookie(c, "", 0)
		platform.Fail(c, http.StatusUnauthorized, "INVALID_SESSION", "The session is invalid or has expired. Sign in again.")
		return
	}
	if revoked != nil {
		// A rotated token came back: it was copied. Revoke the whole sign-in.
		_, _ = tx.Exec(c, `UPDATE dashboard_refresh_tokens SET revoked_at=COALESCE(revoked_at,now()) WHERE family_id=$1`, family)
		_ = tx.Commit(c)
		h.setCookie(c, "", 0)
		platform.Fail(c, http.StatusUnauthorized, "SESSION_REUSE", "This session was used from somewhere else and has been ended. Sign in again.")
		return
	}
	if !h.Now().Before(expires) {
		h.setCookie(c, "", 0)
		platform.Fail(c, http.StatusUnauthorized, "INVALID_SESSION", "The session is invalid or has expired. Sign in again.")
		return
	}
	next := platform.RandomSecret(32)
	var nextID string
	if err = tx.QueryRow(c, `INSERT INTO dashboard_refresh_tokens(user_id,family_id,token_hash,expires_at) VALUES($1,$2,$3,$4) RETURNING id::text`,
		userID, family, platform.Digest(next), h.Now().Add(h.RefreshTTL)).Scan(&nextID); err != nil {
		platform.Internal(c, err)
		return
	}
	if _, err = tx.Exec(c, `UPDATE dashboard_refresh_tokens SET revoked_at=now(),replaced_by=$1 WHERE id=$2`, nextID, tokenID); err != nil {
		platform.Internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		platform.Internal(c, err)
		return
	}
	h.respond(c, http.StatusOK, userID, email, next)
}

// Logout ends the sign-in: every token in the cookie's family is revoked and the cookie
// is cleared. It succeeds without a cookie too, so signing out is always possible.
func (h Handler) Logout(c *gin.Context) {
	if raw, err := c.Cookie(RefreshCookie); err == nil && raw != "" {
		if _, err = h.DB.Exec(c, `UPDATE dashboard_refresh_tokens SET revoked_at=COALESCE(revoked_at,now())
			WHERE family_id=(SELECT family_id FROM dashboard_refresh_tokens WHERE token_hash=$1)`, platform.Digest(raw)); err != nil {
			platform.Internal(c, err)
			return
		}
	}
	h.setCookie(c, "", 0)
	platform.Success(c, http.StatusOK, gin.H{"signed_out": true})
}
