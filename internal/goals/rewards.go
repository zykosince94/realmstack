package goals

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/economy"
	"realmstack-backend/internal/inventory"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/progression"
)

// Source tags for ledger rows written by claims. Goal evaluation counts only "api".
const (
	SourceAchievement = "achievement"
	SourceQuest       = "quest"
)

// ApplyRewards pays r inside tx with the same stack and balance rules as trusted
// grants and credits, tagging every ledger row with source. Any failing reward (for
// example a full stack) returns an error, and the caller's transaction rolls back the
// whole claim. claimRef must be unique per claim; it becomes the ledger reference and
// the XP row's idempotency key. at stamps the ledger rows (the request clock, so goal
// windows compare like with like). Callers must hold the player row lock.
func ApplyRewards(ctx context.Context, tx pgx.Tx, scope platform.Principal, playerID string, r Rewards, source, reason, claimRef string, at time.Time) error {
	for _, x := range r.Items {
		if _, err := inventory.Grant(ctx, tx, scope, playerID, x.ItemKey, x.Quantity, source, reason, claimRef, at); err != nil {
			return unavailable(err)
		}
	}
	for _, x := range r.Currencies {
		if _, err := economy.Credit(ctx, tx, scope, playerID, x.CurrencyKey, x.Amount, source, reason, claimRef, at); err != nil {
			return unavailable(err)
		}
	}
	if r.XP > 0 {
		if _, err := progression.AwardXP(ctx, tx, scope, playerID, r.XP, source, reason, claimRef, claimRef, at); err != nil {
			return unavailable(err)
		}
	}
	return nil
}

// unavailable turns "definition not found" into a 422: the goal is valid, but a reward
// it pays has since been removed from the published catalog.
func unavailable(err error) error {
	var apiErr *platform.Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return &platform.Error{Status: http.StatusUnprocessableEntity, Code: "REWARD_UNAVAILABLE", Message: apiErr.Message}
	}
	return err
}
