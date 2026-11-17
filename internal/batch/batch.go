// Package batch serves POST /v1/server/batch (spec 5.3, 6.6): many trusted writes in
// one request, so a game server can report a match's results without hundreds of
// round trips. Each entry runs the normal idempotency algorithm (platform.MutateIn)
// inside its own savepoint: a failing entry rolls back only itself, a retried batch
// replays the entries that already succeeded, and each player's entries apply in
// request order, so two writes for the same player happen in the order they were sent.
//
// Entries are applied grouped by player ID (a stable sort), not in raw request order:
// a batch holds its row locks until commit, and the load test showed two concurrent
// batches locking random players in opposite orders and deadlocking. Locking players in
// one global order removes that cycle. Results are still reported in request order.
package batch

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"realmstack-backend/internal/economy"
	"realmstack-backend/internal/inventory"
	"realmstack-backend/internal/limits"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/progression"
	"realmstack-backend/internal/stats"
)

// DefaultMaxEntries applies when quotas are not configured (spec: default 500).
const DefaultMaxEntries = 500

// Handler serves the batch route.
type Handler struct {
	*platform.Env
	Quotas *limits.Quotas // nil disables quotas (and uses DefaultMaxEntries)
}

// Entry is one write. Type selects which fields apply.
type Entry struct {
	Type           string `json:"type"` // grant, credit, xp, stat, flag
	IdempotencyKey string `json:"idempotency_key"`
	PlayerID       string `json:"player_id"`
	ItemKey        string `json:"item_key,omitempty"`
	Quantity       int64  `json:"quantity,omitempty"`
	CurrencyKey    string `json:"currency_key,omitempty"`
	Amount         int64  `json:"amount,omitempty"`
	StatKey        string `json:"stat_key,omitempty"`
	FlagKey        string `json:"flag_key,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Reference      string `json:"reference,omitempty"`
}

// Write applies a batch and reports a result or error per entry. The HTTP status is 200
// whenever the batch itself was accepted; each entry carries its own status.
func (h Handler) Write(c *gin.Context) {
	var in struct {
		Entries []Entry `json:"entries"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	actor := platform.GetPrincipal(c)
	max := DefaultMaxEntries
	if h.Quotas != nil {
		max = h.Quotas.Limits(c, actor.ProjectID).MaxBatchEntries
	}
	if len(in.Entries) == 0 || len(in.Entries) > max {
		platform.Fail(c, http.StatusUnprocessableEntity, "BATCH_SIZE", "A batch needs between 1 and the project's max_batch_entries entries.")
		return
	}
	// Every entry counts against the tenant's write quota.
	if h.Quotas != nil {
		if ok, retry := h.Quotas.Take(c, actor.ProjectID, limits.Writes, int64(len(in.Entries))); !ok {
			c.Header("Retry-After", limits.RetryAfterHeader(retry))
			platform.Fail(c, http.StatusTooManyRequests, "QUOTA_EXCEEDED", "The project's write quota is exhausted; retry after the indicated time.")
			return
		}
	}
	raw, err := h.DB.Begin(c)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer raw.Rollback(c)
	tx := platform.Pipelined(raw)
	results := make([]gin.H, len(in.Entries))
	applied := 0
	// UUIDs may arrive in either case; the lock order below and the idempotency keys
	// must see one spelling per player.
	for i := range in.Entries {
		in.Entries[i].PlayerID = strings.ToLower(in.Entries[i].PlayerID)
	}
	order := make([]int, len(in.Entries))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return in.Entries[order[a]].PlayerID < in.Entries[order[b]].PlayerID })
	for _, i := range order {
		e := in.Entries[i]
		status, body, err := h.apply(c, tx, actor, e)
		res := gin.H{"index": i, "idempotency_key": e.IdempotencyKey, "type": e.Type, "status": status}
		if err != nil {
			var apiErr *platform.Error
			if !errors.As(err, &apiErr) {
				h.Log.Error("batch entry failed", "request_id", c.GetString("request_id"), "index", i, "error", err)
				apiErr = classify(err)
			}
			res["status"], res["error"] = apiErr.Status, gin.H{"code": apiErr.Code, "message": apiErr.Message}
		} else {
			res["data"] = body
			applied++
		}
		results[i] = res
	}
	if err = tx.Commit(c); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, gin.H{"results": results, "applied": applied, "failed": len(in.Entries) - applied})
}

// apply runs one entry in its own savepoint with the idempotency algorithm.
func (h Handler) apply(c *gin.Context, tx pgx.Tx, actor platform.Principal, e Entry) (int, json.RawMessage, error) {
	if key := e.IdempotencyKey; key == "" || len(key) > 200 {
		return 0, nil, platform.Errf(http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Every entry needs an idempotency_key (1-200 characters).")
	}
	if !platform.UUIDPattern.MatchString(e.PlayerID) || !platform.PlayerInScope(c, tx, actor, e.PlayerID) {
		return 0, nil, platform.Errf(http.StatusNotFound, "NOT_FOUND", "The player was not found in this environment.")
	}
	fn, ok := h.work(c, e)
	if !ok {
		return 0, nil, platform.Errf(http.StatusBadRequest, "INVALID_REQUEST", "type must be grant, credit, xp, stat or flag.")
	}
	target := actor
	target.ID = e.PlayerID
	// Each entry runs inside its own savepoint. SAVEPOINT and RELEASE are queued, not
	// sent: SAVEPOINT travels with the entry's first statement (the idempotency lock)
	// and RELEASE with the next entry's or the commit. A batch holds its players' row
	// locks until commit, and the load test showed single writes queueing behind
	// batches; two fewer round trips per entry shortens that hold by about a quarter.
	if err := platform.DeferExec(c, tx, `SAVEPOINT batch_entry`); err != nil {
		return 0, nil, err
	}
	payload := e
	payload.IdempotencyKey = "" // the key identifies the request; it is not part of it
	status, body, err := platform.MutateIn(c, tx, target, e.IdempotencyKey, "/v1/server/batch#"+e.Type, payload, fn)
	if err != nil {
		// Only this entry is undone; this also clears an error state the failed
		// statement left on the transaction. The rollback is sent now, so a database
		// error here (the connection is gone) fails the batch instead of being hidden.
		if _, rerr := tx.Exec(c, `ROLLBACK TO SAVEPOINT batch_entry`); rerr != nil {
			return 0, nil, rerr
		}
		if rerr := platform.DeferExec(c, tx, `RELEASE SAVEPOINT batch_entry`); rerr != nil {
			return 0, nil, rerr
		}
		return 0, nil, err
	}
	return status, body, platform.DeferExec(c, tx, `RELEASE SAVEPOINT batch_entry`)
}

// work returns the mutation for an entry, reusing the same transaction functions as
// the single-write routes, so a batch write and a single write behave identically.
func (h Handler) work(c *gin.Context, e Entry) (platform.MutateFunc, bool) {
	scope := platform.GetPrincipal(c)
	scope.ID = e.PlayerID
	now := h.Now()
	switch e.Type {
	case "grant":
		return func(tx pgx.Tx) (int, any, error) {
			owned, err := inventory.Grant(c, tx, scope, e.PlayerID, e.ItemKey, e.Quantity, "api", e.Reason, e.Reference, now)
			return http.StatusOK, gin.H{"item_key": e.ItemKey, "granted": e.Quantity, "quantity": owned}, err
		}, true
	case "credit":
		return func(tx pgx.Tx) (int, any, error) {
			balance, err := economy.Credit(c, tx, scope, e.PlayerID, e.CurrencyKey, e.Amount, "api", e.Reason, e.Reference, now)
			return http.StatusOK, gin.H{"currency_key": e.CurrencyKey, "amount": e.Amount, "balance": balance}, err
		}, true
	case "xp":
		return func(tx pgx.Tx) (int, any, error) {
			if err := platform.LockPlayer(c, tx, scope, e.PlayerID); err != nil {
				return 0, nil, err
			}
			award, err := progression.AwardXP(c, tx, scope, e.PlayerID, e.Amount, "api", e.Reason, e.Reference, e.IdempotencyKey, now)
			return http.StatusOK, award, err
		}, true
	case "stat":
		return func(tx pgx.Tx) (int, any, error) {
			value, err := stats.IncrementTx(c, tx, scope, e.PlayerID, e.StatKey, e.Amount, e.Reason, e.Reference, now)
			return http.StatusOK, gin.H{"stat_key": e.StatKey, "incremented": e.Amount, "value": value}, err
		}, true
	case "flag":
		return func(tx pgx.Tx) (int, any, error) {
			newlySet, err := stats.SetFlagTx(c, tx, scope, e.PlayerID, e.FlagKey, e.Reason, e.Reference, now)
			return http.StatusOK, gin.H{"flag_key": e.FlagKey, "set": true, "newly_set": newlySet}, err
		}, true
	}
	return nil, false
}

// classify turns database errors into entry results. A deadlock or serialization
// failure is retryable; player-ordered entries make deadlocks between batches unlikely,
// but other transactions can still form a cycle with a batch.
func classify(err error) *platform.Error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001") {
		return &platform.Error{Status: http.StatusConflict, Code: "RETRY", Message: "The entry conflicted with a concurrent write; retry it with the same key."}
	}
	return &platform.Error{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "The entry failed unexpectedly."}
}
