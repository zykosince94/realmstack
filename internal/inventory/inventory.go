// Package inventory owns player item stacks and equipment: trusted grants, and
// player self-service consume, equip and unequip.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/totals"
)

// Handler serves inventory routes.
type Handler struct{ *platform.Env }

// Mutation is the body of grant and consume requests.
type Mutation struct {
	ItemKey   string `json:"item_key"`
	Quantity  int64  `json:"quantity"`
	Reason    string `json:"reason"`
	Reference string `json:"reference"`
}

// Slot returns the equipment slot declared in an item's metadata ("slot"), if any.
// Each item has exactly one valid slot, so one stack can never fill two slots.
func Slot(metadata map[string]any) (string, bool) {
	v, ok := metadata["slot"]
	if !ok {
		return "", false
	}
	slot, ok := v.(string)
	return slot, ok
}

// Grant adds quantity of itemKey to the player's stack inside tx and writes the
// ledger row tagged with source ("api", "achievement" or "quest"). The conflict update
// locks the stack and re-checks the limit, so concurrent grants can never exceed
// max_stack. It returns the new stack size.
func Grant(ctx context.Context, tx pgx.Tx, scope platform.Principal, playerID, itemKey string, quantity int64, source, reason, reference string, at time.Time) (int64, error) {
	if quantity <= 0 || quantity > 1e9 {
		return 0, platform.Errf(http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Quantity must be positive and within limits.")
	}
	var itemID string
	var maxStack int64
	err := tx.QueryRow(ctx, `SELECT id::text,max_stack FROM item_definitions WHERE project_id=$1 AND environment_id=$2 AND item_key=$3 AND active`, scope.ProjectID, scope.EnvironmentID, itemKey).Scan(&itemID, &maxStack)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, platform.Errf(http.StatusNotFound, "NOT_FOUND", "Item %q is not a published item.", itemKey)
	}
	if err != nil {
		return 0, err
	}
	var after int64
	if quantity <= maxStack {
		err = tx.QueryRow(ctx, `INSERT INTO inventory(player_id,project_id,environment_id,item_id,quantity) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(player_id,item_id) DO UPDATE SET quantity=inventory.quantity+EXCLUDED.quantity,updated_at=now()
			WHERE inventory.quantity+EXCLUDED.quantity<=$6 RETURNING quantity`, playerID, scope.ProjectID, scope.EnvironmentID, itemID, quantity, maxStack).Scan(&after)
	} else {
		err = pgx.ErrNoRows
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, platform.Errf(http.StatusUnprocessableEntity, "STACK_LIMIT", "Granting %d %s would exceed its stack limit of %d.", quantity, itemKey, maxStack)
	}
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO economy_transactions(player_id,project_id,environment_id,item_id,operation,quantity,reason,reference,source,created_at) VALUES($1,$2,$3,$4,'grant',$5,$6,$7,$8,$9)`,
		playerID, scope.ProjectID, scope.EnvironmentID, itemID, quantity, reason, reference, source, at)
	if err != nil || source != "api" {
		return after, err // only api rows count toward goals, so only they move the totals
	}
	return after, totals.Add(ctx, tx, scope, playerID, at, totals.Delta{Metric: totals.Grant(itemKey), Amount: quantity}, totals.Delta{Metric: totals.GrantAny, Amount: quantity})
}

// GrantHandler is the trusted-server grant route.
func (h Handler) GrantHandler(c *gin.Context) {
	var in Mutation
	if !platform.Bind(c, &in) {
		return
	}
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	h.Mutate(c, actor, in, func(tx pgx.Tx) (int, any, error) {
		owned, err := Grant(c, tx, actor, actor.ID, in.ItemKey, in.Quantity, "api", in.Reason, in.Reference, h.Now())
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, gin.H{"item_key": in.ItemKey, "operation": "grant", "quantity": in.Quantity, "owned": owned}, nil
	})
}

// List returns the player's non-empty stacks with their equipped slot.
func (h Handler) List(c *gin.Context) {
	p := platform.GetPrincipal(c)
	rows, err := h.DB.Query(c, `SELECT d.item_key,d.name,d.category,i.quantity,d.metadata,COALESCE(e.slot_key,'') FROM inventory i JOIN item_definitions d ON d.id=i.item_id
		LEFT JOIN equipment e ON e.player_id=i.player_id AND e.item_id=i.item_id
		WHERE i.player_id=$1 AND i.project_id=$2 AND i.environment_id=$3 AND i.quantity>0 ORDER BY d.item_key`, p.ID, p.ProjectID, p.EnvironmentID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var key, name, category, slot string
		var qty int64
		var meta []byte
		if err = rows.Scan(&key, &name, &category, &qty, &meta, &slot); err != nil {
			platform.Internal(c, err)
			return
		}
		out = append(out, gin.H{"item_key": key, "name": name, "category": category, "quantity": qty, "metadata": json.RawMessage(meta), "equipped_slot": slot})
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}

// Get returns one owned stack.
func (h Handler) Get(c *gin.Context) {
	p := platform.GetPrincipal(c)
	var name, category, slot string
	var qty int64
	var meta []byte
	err := h.DB.QueryRow(c, `SELECT d.name,d.category,i.quantity,d.metadata,COALESCE(e.slot_key,'') FROM inventory i JOIN item_definitions d ON d.id=i.item_id
		LEFT JOIN equipment e ON e.player_id=i.player_id AND e.item_id=i.item_id
		WHERE i.player_id=$1 AND i.project_id=$2 AND i.environment_id=$3 AND d.item_key=$4`, p.ID, p.ProjectID, p.EnvironmentID, c.Param("itemKey")).Scan(&name, &category, &qty, &meta, &slot)
	if errors.Is(err, pgx.ErrNoRows) {
		platform.NotFound(c)
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, gin.H{"item_key": c.Param("itemKey"), "name": name, "category": category, "quantity": qty, "metadata": json.RawMessage(meta), "equipped_slot": slot})
}

// Consume removes quantity from the player's own stack; using up the last unit of an
// equipped item unequips it.
func (h Handler) Consume(c *gin.Context) {
	var in Mutation
	if !platform.Bind(c, &in) {
		return
	}
	if in.Quantity <= 0 || in.Quantity > 1e9 {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Quantity must be positive.")
		return
	}
	p := platform.GetPrincipal(c)
	h.Mutate(c, p, in, func(tx pgx.Tx) (int, any, error) {
		var itemID string
		var after int64
		err := tx.QueryRow(c, `UPDATE inventory i SET quantity=i.quantity-$5,updated_at=now() FROM item_definitions d
			WHERE d.id=i.item_id AND i.player_id=$1 AND i.project_id=$2 AND i.environment_id=$3 AND d.item_key=$4 AND i.quantity>=$5
			RETURNING i.item_id::text,i.quantity`, p.ID, p.ProjectID, p.EnvironmentID, in.ItemKey, in.Quantity).Scan(&itemID, &after)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "INSUFFICIENT_ITEMS", "The player does not own enough items.")
		}
		if err != nil {
			return 0, nil, err
		}
		unequipped := false
		if after == 0 {
			tag, err := tx.Exec(c, `DELETE FROM equipment WHERE player_id=$1 AND item_id=$2`, p.ID, itemID)
			if err != nil {
				return 0, nil, err
			}
			unequipped = tag.RowsAffected() > 0
		}
		now := h.Now()
		if _, err = tx.Exec(c, `INSERT INTO economy_transactions(player_id,project_id,environment_id,item_id,operation,quantity,reason,reference,created_at) VALUES($1,$2,$3,$4,'consume',$5,$6,$7,$8)`,
			p.ID, p.ProjectID, p.EnvironmentID, itemID, in.Quantity, in.Reason, in.Reference, now); err != nil {
			return 0, nil, err
		}
		if err = totals.Add(c, tx, p, p.ID, now, totals.Delta{Metric: totals.Consume(in.ItemKey), Amount: in.Quantity}, totals.Delta{Metric: totals.ConsumeAny, Amount: in.Quantity}); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, gin.H{"item_key": in.ItemKey, "consumed": in.Quantity, "remaining": after, "unequipped": unequipped}, nil
	})
}

// Equip puts an owned item in its declared slot.
func (h Handler) Equip(c *gin.Context) {
	var in struct {
		ItemKey string `json:"item_key"`
		Slot    string `json:"slot"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	if !platform.KeyPattern.MatchString(in.Slot) {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "A valid slot is required.")
		return
	}
	p := platform.GetPrincipal(c)
	h.Mutate(c, p, in, func(tx pgx.Tx) (int, any, error) {
		var itemID string
		var rawMeta []byte
		err := tx.QueryRow(c, `SELECT i.item_id::text,d.metadata FROM inventory i JOIN item_definitions d ON d.id=i.item_id
			WHERE i.player_id=$1 AND i.project_id=$2 AND i.environment_id=$3 AND d.item_key=$4 AND i.quantity>0 FOR UPDATE OF i`, p.ID, p.ProjectID, p.EnvironmentID, in.ItemKey).Scan(&itemID, &rawMeta)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "ITEM_NOT_OWNED", "The player does not own that item.")
		}
		if err != nil {
			return 0, nil, err
		}
		var meta map[string]any
		_ = json.Unmarshal(rawMeta, &meta)
		slot, equippable := Slot(meta)
		if !equippable {
			return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "ITEM_NOT_EQUIPPABLE", "That item cannot be equipped.")
		}
		if slot != in.Slot {
			return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "INVALID_SLOT", "That item can only be equipped in the %q slot.", slot)
		}
		if _, err = tx.Exec(c, `INSERT INTO equipment(player_id,project_id,environment_id,slot_key,item_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT(player_id,slot_key) DO UPDATE SET item_id=EXCLUDED.item_id`,
			p.ID, p.ProjectID, p.EnvironmentID, in.Slot, itemID); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, gin.H{"slot": in.Slot, "item_key": in.ItemKey}, nil
	})
}

// Unequip empties a slot.
func (h Handler) Unequip(c *gin.Context) {
	var in struct {
		Slot string `json:"slot"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	if !platform.KeyPattern.MatchString(in.Slot) {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "A valid slot is required.")
		return
	}
	p := platform.GetPrincipal(c)
	h.Mutate(c, p, in, func(tx pgx.Tx) (int, any, error) {
		tag, err := tx.Exec(c, `DELETE FROM equipment WHERE player_id=$1 AND project_id=$2 AND environment_id=$3 AND slot_key=$4`, p.ID, p.ProjectID, p.EnvironmentID, in.Slot)
		if err != nil {
			return 0, nil, err
		}
		if tag.RowsAffected() == 0 {
			return 0, nil, platform.Errf(http.StatusNotFound, "NOT_FOUND", "Nothing is equipped in that slot.")
		}
		return http.StatusOK, gin.H{"slot": in.Slot, "equipped": false}, nil
	})
}
