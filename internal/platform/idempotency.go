package platform

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// IdempotencyKey returns the trimmed Idempotency-Key header, or "" when missing or too long.
func IdempotencyKey(c *gin.Context) string {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(key) < 1 || len(key) > 200 {
		return ""
	}
	return key
}

// IdempotencyKind is the idempotency namespace for an actor: "server" for trusted
// server credentials, "player" otherwise.
func IdempotencyKind(actor Principal) string {
	if actor.Kind == KindServer {
		return "server"
	}
	return "player"
}

// MutateFunc does the work of an idempotent mutation inside tx. It returns the HTTP
// status and result to send and store, or an error (an *Error for API errors). A
// returned error rolls the transaction back and nothing is stored, so a retry with the
// same key re-evaluates (for example after the player earns enough currency).
type MutateFunc func(tx pgx.Tx) (status int, result any, err error)

// Mutate runs fn in one transaction guarded by the request's Idempotency-Key.
//
// actor.ID is the player the mutation targets; actor.Kind namespaces the key, so a
// player's self-service keys and the trusted server's keys for that player never
// collide. The request hash covers route, actor, kind and payload, but not which
// server credential was used, so a retry after credential rotation replays instead
// of conflicting. A replay returns the stored result with its original HTTP status;
// the same key with a different request returns 409.
func (e *Env) Mutate(c *gin.Context, actor Principal, payload any, fn MutateFunc) {
	key := IdempotencyKey(c)
	if key == "" {
		Fail(c, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required.")
		return
	}
	tx, err := e.DB.Begin(c)
	if err != nil {
		Internal(c, err)
		return
	}
	defer tx.Rollback(c)
	kind := IdempotencyKind(actor)
	// Serialize concurrent uses of one key; the primary key is the final guard.
	if _, err = tx.Exec(c, `SELECT pg_advisory_xact_lock(hashtext($1))`, actor.ProjectID+":"+actor.EnvironmentID+":"+kind+":"+actor.ID+":"+key); err != nil {
		Internal(c, err)
		return
	}
	body, _ := json.Marshal(gin.H{"route": c.FullPath(), "actor": actor.ID, "kind": kind, "payload": payload})
	hash := Digest(string(body))
	var prior string
	var stored []byte
	var storedStatus int
	err = tx.QueryRow(c, `SELECT request_hash,result,status_code FROM idempotency_records WHERE project_id=$1 AND environment_id=$2 AND actor_kind=$3 AND actor_id=$4 AND key=$5`,
		actor.ProjectID, actor.EnvironmentID, kind, actor.ID, key).Scan(&prior, &stored, &storedStatus)
	switch {
	case err == nil && prior != hash:
		Fail(c, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency key was already used with a different request.")
		return
	case err == nil:
		Success(c, storedStatus, json.RawMessage(stored))
		return
	case !errors.Is(err, pgx.ErrNoRows):
		Internal(c, err)
		return
	}
	status, result, err := fn(tx)
	if err != nil {
		Respond(c, err)
		return
	}
	b, err := json.Marshal(result)
	if err != nil {
		Internal(c, err)
		return
	}
	if _, err = tx.Exec(c, `INSERT INTO idempotency_records(project_id,environment_id,actor_kind,actor_id,key,request_hash,result,status_code) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		actor.ProjectID, actor.EnvironmentID, kind, actor.ID, key, hash, b, status); err != nil {
		Internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		Internal(c, err)
		return
	}
	Success(c, status, result)
}
