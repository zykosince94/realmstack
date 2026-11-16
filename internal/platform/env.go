package platform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Env is the shared dependency set handed to every domain handler. Domain handlers
// embed *Env, so tests can replace Now once and every package sees the same clock.
type Env struct {
	DB         *pgxpool.Pool
	Log        *slog.Logger
	JWTSecret  string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	// Now is the single clock for token expiry, refresh expiry, season checks and
	// stored timestamps that are later compared. Tests replace it to test boundaries.
	Now func() time.Time
}

// Querier is satisfied by *pgxpool.Pool, *pgxpool.Conn and pgx.Tx, so read helpers
// work inside or outside a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Principal is the authenticated actor. For server credentials ID is the key prefix;
// for user and player tokens it is the subject UUID.
type Principal struct {
	ID, ProjectID, EnvironmentID, Kind string
}

const (
	KindUser   = "user"
	KindPlayer = "player"
	KindServer = "server"
	KindPublic = "public"
)

// SetPrincipal stores the authenticated principal on the request.
func SetPrincipal(c *gin.Context, p Principal) { c.Set("principal", p) }

// GetPrincipal returns the authenticated principal set by auth middleware.
func GetPrincipal(c *gin.Context) Principal {
	p, _ := c.Get("principal")
	v, _ := p.(Principal)
	return v
}

// HasPrincipal reports whether the request carries an authenticated principal.
func HasPrincipal(c *gin.Context) bool { _, ok := c.Get("principal"); return ok }

// SetScope stores the tenant scope of a public (or player) request.
func SetScope(c *gin.Context, p Principal) { c.Set("scope", p) }

// GetScope returns the tenant scope set by the public-scope middleware.
func GetScope(c *gin.Context) Principal {
	v, _ := c.Get("scope")
	p, _ := v.(Principal)
	return p
}

// Token issues a compact HS256 JWT for p that expires after ttl.
func (e *Env) Token(p Principal, ttl time.Duration) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(gin.H{"sub": p.ID, "kind": p.Kind, "project_id": p.ProjectID, "environment_id": p.EnvironmentID, "exp": e.Now().Add(ttl).Unix()})
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + hmacText(e.JWTSecret, body)
}

// ReadToken verifies a "Bearer <jwt>" header and returns its principal if the token
// is validly signed, unexpired and of the expected kind. The signature is always
// computed with HS256, so the header's alg cannot downgrade verification.
func (e *Env) ReadToken(header, kind string) (Principal, bool) {
	var p Principal
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return p, false
	}
	t := strings.Split(parts[1], ".")
	if len(t) != 3 {
		return p, false
	}
	if subtle.ConstantTimeCompare([]byte(hmacText(e.JWTSecret, t[0]+"."+t[1])), []byte(t[2])) != 1 {
		return p, false
	}
	b, err := base64.RawURLEncoding.DecodeString(t[1])
	if err != nil {
		return p, false
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return p, false
	}
	exp, ok := claims["exp"].(float64)
	if !ok || e.Now().Unix() >= int64(exp) {
		return p, false
	}
	p.ID, _ = claims["sub"].(string)
	p.ProjectID, _ = claims["project_id"].(string)
	p.EnvironmentID, _ = claims["environment_id"].(string)
	p.Kind, _ = claims["kind"].(string)
	return p, p.ID != "" && p.Kind == kind
}

func hmacText(secret, msg string) string {
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
