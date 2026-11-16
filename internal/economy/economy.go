// Package economy owns currency balances and the economy history: trusted credits,
// player spends, balance reads and the cursor-paginated transaction log.
package economy

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// Handler serves currency routes.
type Handler struct{ *platform.Env }

// MaxBalance caps any balance well inside the int64 range.
const MaxBalance = 9_000_000_000_000_000

// Mutation is the body of credit and spend requests.
type Mutation struct {
	CurrencyKey string `json:"currency_key"`
	Amount      int64  `json:"amount"`
	Reason      string `json:"reason"`
	Reference   string `json:"reference"`
}

func currencyID(ctx context.Context, tx pgx.Tx, scope platform.Principal, key string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM currency_definitions WHERE project_id=$1 AND environment_id=$2 AND currency_key=$3 AND active`, scope.ProjectID, scope.EnvironmentID, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", platform.Errf(http.StatusNotFound, "NOT_FOUND", "Currency %q is not a published currency.", key)
	}
	return id, err
}

func validAmount(amount int64) error {
	if amount <= 0 || amount > 1e12 {
		return platform.Errf(http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Amount must be positive and within limits.")
	}
	return nil
}

// Credit adds amount to the player's balance inside tx and writes the ledger row
// tagged with source. One atomic upsert: the conflict update locks the balance row and
// re-checks the cap. It returns the new balance.
func Credit(ctx context.Context, tx pgx.Tx, scope platform.Principal, playerID, currencyKey string, amount int64, source, reason, reference string) (int64, error) {
	if err := validAmount(amount); err != nil {
		return 0, err
	}
	id, err := currencyID(ctx, tx, scope, currencyKey)
	if err != nil {
		return 0, err
	}
	var after int64
	err = tx.QueryRow(ctx, `INSERT INTO balances(player_id,project_id,environment_id,currency_id,amount) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(player_id,currency_id) DO UPDATE SET amount=balances.amount+EXCLUDED.amount,updated_at=now()
		WHERE balances.amount+EXCLUDED.amount<=$6 RETURNING amount`, playerID, scope.ProjectID, scope.EnvironmentID, id, amount, MaxBalance).Scan(&after)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, platform.Errf(http.StatusUnprocessableEntity, "BALANCE_LIMIT", "Balance would exceed supported limits.")
	}
	if err != nil {
		return 0, err
	}
	return after, insertLedger(ctx, tx, scope, playerID, id, "credit", amount, after, source, reason, reference)
}

// Spend removes amount from the player's balance inside tx. The WHERE clause is the
// funds check and is re-evaluated after the row lock, so concurrent spends can never
// overdraw. It returns the new balance.
func Spend(ctx context.Context, tx pgx.Tx, scope platform.Principal, playerID, currencyKey string, amount int64, reason, reference string) (int64, error) {
	if err := validAmount(amount); err != nil {
		return 0, err
	}
	id, err := currencyID(ctx, tx, scope, currencyKey)
	if err != nil {
		return 0, err
	}
	var after int64
	err = tx.QueryRow(ctx, `UPDATE balances SET amount=amount-$3,updated_at=now() WHERE player_id=$1 AND currency_id=$2 AND amount>=$3 RETURNING amount`, playerID, id, amount).Scan(&after)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, platform.Errf(http.StatusUnprocessableEntity, "INSUFFICIENT_FUNDS", "The player does not have enough currency.")
	}
	if err != nil {
		return 0, err
	}
	return after, insertLedger(ctx, tx, scope, playerID, id, "spend", amount, after, "api", reason, reference)
}

func insertLedger(ctx context.Context, tx pgx.Tx, scope platform.Principal, playerID, currencyID, operation string, amount, after int64, source, reason, reference string) error {
	_, err := tx.Exec(ctx, `INSERT INTO economy_transactions(player_id,project_id,environment_id,currency_id,operation,quantity,balance_after,reason,reference,source) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		playerID, scope.ProjectID, scope.EnvironmentID, currencyID, operation, amount, after, reason, reference, source)
	return err
}

func result(in Mutation, operation string, balance int64) gin.H {
	return gin.H{"currency_key": in.CurrencyKey, "operation": operation, "amount": in.Amount, "balance": balance}
}

// CreditHandler is the trusted-server credit route.
func (h Handler) CreditHandler(c *gin.Context) {
	var in Mutation
	if !platform.Bind(c, &in) {
		return
	}
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	h.Mutate(c, actor, in, func(tx pgx.Tx) (int, any, error) {
		balance, err := Credit(c, tx, actor, actor.ID, in.CurrencyKey, in.Amount, "api", in.Reason, in.Reference)
		return http.StatusOK, result(in, "credit", balance), err
	})
}

// SpendHandler is the player self-service spend route.
func (h Handler) SpendHandler(c *gin.Context) {
	var in Mutation
	if !platform.Bind(c, &in) {
		return
	}
	if err := validAmount(in.Amount); err != nil {
		platform.Respond(c, err)
		return
	}
	p := platform.GetPrincipal(c)
	h.Mutate(c, p, in, func(tx pgx.Tx) (int, any, error) {
		balance, err := Spend(c, tx, p, p.ID, in.CurrencyKey, in.Amount, in.Reason, in.Reference)
		return http.StatusOK, result(in, "spend", balance), err
	})
}

// Balances lists every active currency with the player's balance (0 when none).
func (h Handler) Balances(c *gin.Context) {
	p := platform.GetPrincipal(c)
	rows, err := h.DB.Query(c, `SELECT d.currency_key,d.name,COALESCE(b.amount,0) FROM currency_definitions d LEFT JOIN balances b ON b.currency_id=d.id AND b.player_id=$1
		WHERE d.project_id=$2 AND d.environment_id=$3 AND d.active ORDER BY d.currency_key`, p.ID, p.ProjectID, p.EnvironmentID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var key, name string
		var amount int64
		if err = rows.Scan(&key, &name, &amount); err != nil {
			platform.Internal(c, err)
			return
		}
		out = append(out, gin.H{"currency_key": key, "name": name, "balance": amount})
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}

// Transactions returns economy history newest first. Pass meta.next_cursor from one
// page as ?cursor= to read the next; it is absent on the last page.
func (h Handler) Transactions(c *gin.Context) {
	p := platform.GetPrincipal(c)
	limit := 50
	if x, e := strconv.Atoi(c.Query("limit")); e == nil && x > 0 && x <= 100 {
		limit = x
	}
	var beforeAt *time.Time
	var beforeID *string
	if cursor := c.Query("cursor"); cursor != "" {
		at, id, ok := DecodeCursor(cursor)
		if !ok {
			platform.Fail(c, http.StatusBadRequest, "INVALID_CURSOR", "The pagination cursor is invalid.")
			return
		}
		beforeAt, beforeID = &at, &id
	}
	rows, err := h.DB.Query(c, `SELECT t.id::text,t.operation,t.quantity,COALESCE(t.balance_after,0),COALESCE(d.currency_key,''),COALESCE(i.item_key,''),t.reason,t.reference,t.source,t.created_at
		FROM economy_transactions t LEFT JOIN currency_definitions d ON d.id=t.currency_id LEFT JOIN item_definitions i ON i.id=t.item_id
		WHERE t.player_id=$1 AND t.project_id=$2 AND t.environment_id=$3 AND ($4::timestamptz IS NULL OR (t.created_at,t.id) < ($4::timestamptz,$5::uuid))
		ORDER BY t.created_at DESC,t.id DESC LIMIT $6`, p.ID, p.ProjectID, p.EnvironmentID, beforeAt, beforeID, limit+1)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	var lastAt time.Time
	var lastID string
	more := false
	for rows.Next() {
		if len(out) == limit {
			more = true
			break
		}
		var id, op, cur, item, reason, ref, source string
		var amount, balance int64
		var at time.Time
		if err = rows.Scan(&id, &op, &amount, &balance, &cur, &item, &reason, &ref, &source, &at); err != nil {
			platform.Internal(c, err)
			return
		}
		lastAt, lastID = at, id
		out = append(out, gin.H{"id": id, "operation": op, "amount": amount, "balance": balance, "currency_key": cur, "item_key": item, "reason": reason, "reference": ref, "source": source, "created_at": at})
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	var meta gin.H
	if more {
		meta = gin.H{"next_cursor": EncodeCursor(lastAt, lastID)}
	}
	platform.SuccessMeta(c, http.StatusOK, out, meta)
}

// EncodeCursor packs a (created_at, id) position into an opaque cursor.
func EncodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

// DecodeCursor reverses EncodeCursor, rejecting anything malformed.
func DecodeCursor(cursor string) (time.Time, string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", false
	}
	at, id, found := strings.Cut(string(b), "|")
	t, err := time.Parse(time.RFC3339Nano, at)
	if !found || err != nil || !platform.UUIDPattern.MatchString(id) {
		return time.Time{}, "", false
	}
	return t, id, true
}
