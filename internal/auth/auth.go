// Package auth registers and signs in dashboard users (game developers).
package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// Handler serves the dashboard authentication routes.
type Handler struct{ *platform.Env }

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h Handler) session(c *gin.Context, status int, id, email string) {
	platform.Success(c, status, gin.H{"user": gin.H{"id": id, "email": email}, "access_token": h.Token(platform.Principal{ID: id, Kind: platform.KindUser}, h.AccessTTL), "token_type": "Bearer"})
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
