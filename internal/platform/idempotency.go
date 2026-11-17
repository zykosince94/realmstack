package platform

import (
	"context"
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
	status, result, err := MutateIn(c, tx, actor, key, c.FullPath(), payload, fn)
	if err != nil {
		Respond(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		Internal(c, err)
		return
	}
	Success(c, status, result)
}

// MutateIn runs the idempotency algorithm inside an existing transaction: lock the
// key, replay a stored result or reject a reused key with a different request (409),
// otherwise run fn and store its result. Mutate wraps it for one request; batch writes
// call it once per entry inside a savepoint, so every entry gets exactly the same
// guarantees as a single call. route is part of the request hash.
func MutateIn(ctx context.Context, tx pgx.Tx, actor Principal, key, route string, payload any, fn MutateFunc) (int, json.RawMessage, error) {
	kind := IdempotencyKind(actor)
	body, _ := json.Marshal(gin.H{"route": route, "actor": actor.ID, "kind": kind, "payload": payload})
	hash := Digest(string(body))
	// One round trip for both: the lock serializes concurrent uses of one key (the
	// primary key is the final guard) and the lookup runs after it, in pipeline order.
	b := &pgx.Batch{}
	b.Queue(`SELECT pg_advisory_xact_lock(hashtext($1))`, actor.ProjectID+":"+actor.EnvironmentID+":"+kind+":"+actor.ID+":"+key)
	b.Queue(`SELECT request_hash,result,status_code FROM idempotency_records WHERE project_id=$1 AND environment_id=$2 AND actor_kind=$3 AND actor_id=$4 AND key=$5`,
		actor.ProjectID, actor.EnvironmentID, kind, actor.ID, key)
	br := tx.SendBatch(ctx, b)
	var prior string
	var stored []byte
	var storedStatus int
	_, err := br.Exec()
	if err == nil {
		err = br.QueryRow().Scan(&prior, &stored, &storedStatus)
	}
	if cerr := br.Close(); cerr != nil && (err == nil || errors.Is(err, pgx.ErrNoRows)) {
		err = cerr
	}
	switch {
	case err == nil && prior != hash:
		return 0, nil, &Error{Status: http.StatusConflict, Code: "IDEMPOTENCY_CONFLICT", Message: "Idempotency key was already used with a different request."}
	case err == nil:
		return storedStatus, stored, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, nil, err
	}
	// Ledger rows and totals that fn defers are sent with the record insert below.
	tx = newPipeTx(tx)
	status, result, err := fn(tx)
	if err != nil {
		return 0, nil, err
	}
	out, err := json.Marshal(result)
	if err != nil {
		return 0, nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO idempotency_records(project_id,environment_id,actor_kind,actor_id,key,request_hash,result,status_code) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		actor.ProjectID, actor.EnvironmentID, kind, actor.ID, key, hash, out, status); err != nil {
		return 0, nil, err
	}
	return status, out, nil
}
