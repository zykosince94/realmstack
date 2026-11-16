// Package goals is the shared goal engine used by achievements and quests: a
// closed set of declarative conditions, their validation, deterministic
// evaluation from the immutable ledgers, and transactional reward application.
//
// There are no scripts or expressions. Progress is derived only from ledger rows
// tagged source='api', so rewards paid by one claim can never complete another goal.
package goals

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"realmstack-backend/internal/platform"
)

// Condition types (spec 2.6). The set is closed: anything else is rejected.
const (
	XPTotal          = "xp_total"
	LevelReached     = "level_reached"
	ItemGranted      = "item_granted"
	ItemConsumed     = "item_consumed"
	CurrencyEarned   = "currency_earned"
	CurrencySpent    = "currency_spent"
	LeaderboardScore = "leaderboard_score"
	Stat             = "stat"
	Flag             = "flag"
)

// fieldsByType lists the fields each condition type accepts besides "type".
var fieldsByType = map[string][]string{
	XPTotal:          {"target"},
	LevelReached:     {"target"},
	ItemGranted:      {"item_key", "target"},
	ItemConsumed:     {"item_key", "target"},
	CurrencyEarned:   {"currency_key", "target"},
	CurrencySpent:    {"currency_key", "target"},
	LeaderboardScore: {"leaderboard_key", "target"},
	Stat:             {"stat_key", "target"},
	Flag:             {"flag_key"},
}

// Types returns the condition types in a stable order.
func Types() []string {
	out := make([]string, 0, len(fieldsByType))
	for t := range fieldsByType {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Condition is one declarative goal condition.
type Condition struct {
	Type           string `json:"type"`
	ItemKey        string `json:"item_key,omitempty"`
	CurrencyKey    string `json:"currency_key,omitempty"`
	LeaderboardKey string `json:"leaderboard_key,omitempty"`
	StatKey        string `json:"stat_key,omitempty"`
	FlagKey        string `json:"flag_key,omitempty"`
	Target         int64  `json:"target,omitempty"`

	// present records which JSON fields the definition actually supplied, so
	// validation can reject fields that do not belong to the condition type.
	present []string
}

// UnmarshalJSON decodes a condition and remembers which fields were present.
func (c *Condition) UnmarshalJSON(b []byte) error {
	type plain Condition // same fields, no methods: avoids infinite recursion
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	*c = Condition(p)
	c.present = c.present[:0]
	for k := range keys {
		c.present = append(c.present, k)
	}
	sort.Strings(c.present)
	return nil
}

// ItemReward, CurrencyReward and Rewards describe what a claim pays.
type ItemReward struct {
	ItemKey  string `json:"item_key"`
	Quantity int64  `json:"quantity"`
}
type CurrencyReward struct {
	CurrencyKey string `json:"currency_key"`
	Amount      int64  `json:"amount"`
}
type Rewards struct {
	Items      []ItemReward     `json:"items,omitempty"`
	Currencies []CurrencyReward `json:"currencies,omitempty"`
	XP         int64            `json:"xp,omitempty"`
}

// Empty reports whether the rewards pay nothing.
func (r Rewards) Empty() bool { return len(r.Items) == 0 && len(r.Currencies) == 0 && r.XP == 0 }

// Refs is what conditions and rewards may reference. A nil *Refs skips reference
// checks (used when one definition is edited on its own); publish passes the full set.
type Refs struct {
	Items        map[string]int64  // item key → max stack
	Currencies   map[string]bool   //
	Leaderboards map[string]bool   //
	Stats        map[string]string // stat key → "counter" | "flag"
	MaxLevel     int               // highest reachable level (len(thresholds)+1)
}

// ValidateConditions checks 1–max conditions at path (e.g. "achievements[2].conditions").
func ValidateConditions(path string, conds []Condition, max int, refs *Refs, errs *platform.FieldErrors) {
	if len(conds) < 1 || len(conds) > max {
		errs.Add(path, "needs 1 to %d conditions", max)
	}
	for i, c := range conds {
		validateCondition(fmt.Sprintf("%s[%d]", path, i), c, refs, errs)
	}
}

func validateCondition(path string, c Condition, refs *Refs, errs *platform.FieldErrors) {
	allowed, ok := fieldsByType[c.Type]
	if !ok {
		errs.Add(path+".type", "must be one of %v", Types())
		return
	}
	for _, f := range c.present {
		if f != "type" && !slices.Contains(allowed, f) {
			errs.Add(path+"."+f, "is not a field of a %s condition", c.Type)
		}
	}
	if c.Type != Flag && c.Target <= 0 {
		errs.Add(path+".target", "must be a positive integer")
	}
	switch c.Type {
	case ItemGranted, ItemConsumed:
		// An omitted item_key means any item.
		if c.ItemKey != "" && refs != nil {
			if _, ok := refs.Items[c.ItemKey]; !ok {
				errs.Add(path+".item_key", "references unknown item %q", c.ItemKey)
			}
		}
	case CurrencyEarned, CurrencySpent:
		switch {
		case c.CurrencyKey == "":
			errs.Add(path+".currency_key", "is required")
		case refs != nil && !refs.Currencies[c.CurrencyKey]:
			errs.Add(path+".currency_key", "references unknown currency %q", c.CurrencyKey)
		}
	case LeaderboardScore:
		switch {
		case c.LeaderboardKey == "":
			errs.Add(path+".leaderboard_key", "is required")
		case refs != nil && !refs.Leaderboards[c.LeaderboardKey]:
			errs.Add(path+".leaderboard_key", "references unknown leaderboard %q", c.LeaderboardKey)
		}
	case Stat:
		validateStatRef(path+".stat_key", c.StatKey, "counter", refs, errs)
	case Flag:
		validateStatRef(path+".flag_key", c.FlagKey, "flag", refs, errs)
	case LevelReached:
		if refs != nil && c.Target > int64(refs.MaxLevel) {
			errs.Add(path+".target", "level %d is unreachable; the progression curve ends at level %d", c.Target, refs.MaxLevel)
		}
	}
}

func validateStatRef(path, key, kind string, refs *Refs, errs *platform.FieldErrors) {
	if key == "" {
		errs.Add(path, "is required")
		return
	}
	if refs == nil {
		return
	}
	got, ok := refs.Stats[key]
	switch {
	case !ok:
		errs.Add(path, "references unknown %s %q", kind, key)
	case got != kind:
		errs.Add(path, "%q is a %s, not a %s", key, got, kind)
	}
}

// ValidateRewards checks reward amounts and references at path.
func ValidateRewards(path string, r Rewards, refs *Refs, errs *platform.FieldErrors) {
	seen := map[string]bool{}
	for i, x := range r.Items {
		p := fmt.Sprintf("%s.items[%d]", path, i)
		if seen["i"+x.ItemKey] {
			errs.Add(p+".item_key", "duplicates another item reward")
		}
		seen["i"+x.ItemKey] = true
		if x.Quantity <= 0 {
			errs.Add(p+".quantity", "must be positive")
		}
		if refs != nil {
			maxStack, ok := refs.Items[x.ItemKey]
			switch {
			case !ok:
				errs.Add(p+".item_key", "references unknown item %q", x.ItemKey)
			case x.Quantity > maxStack:
				errs.Add(p+".quantity", "exceeds the item's max stack of %d", maxStack)
			}
		}
	}
	for i, x := range r.Currencies {
		p := fmt.Sprintf("%s.currencies[%d]", path, i)
		if seen["c"+x.CurrencyKey] {
			errs.Add(p+".currency_key", "duplicates another currency reward")
		}
		seen["c"+x.CurrencyKey] = true
		if x.Amount <= 0 || x.Amount > 1e12 {
			errs.Add(p+".amount", "must be positive and within limits")
		}
		if refs != nil && !refs.Currencies[x.CurrencyKey] {
			errs.Add(p+".currency_key", "references unknown currency %q", x.CurrencyKey)
		}
	}
	if r.XP < 0 || r.XP > 1e9 {
		errs.Add(path+".xp", "must be between 0 and 1000000000")
	}
}
