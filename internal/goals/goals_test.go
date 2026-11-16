package goals

import (
	"encoding/json"
	"strings"
	"testing"

	"realmstack-backend/internal/platform"
)

func conditions(t *testing.T, raw string) []Condition {
	t.Helper()
	var out []Condition
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func refs() *Refs {
	return &Refs{
		Items:        map[string]int64{"iron_ore": 50, "sword": 1},
		Currencies:   map[string]bool{"credits": true},
		Leaderboards: map[string]bool{"speedrun": true},
		Stats:        map[string]string{"trolls": "counter", "met_elder": "flag"},
		MaxLevel:     4,
	}
}

func fieldPaths(errs platform.FieldErrors) string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Field
	}
	return strings.Join(out, ",")
}

func TestValidateConditions(t *testing.T) {
	tests := []struct {
		name, conds, want string
	}{
		{"valid set", `[{"type":"item_granted","target":5},{"type":"stat","stat_key":"trolls","target":3},{"type":"flag","flag_key":"met_elder"}]`, ""},
		{"unknown type", `[{"type":"kill_dragon","target":1}]`, "c[0].type"},
		{"field from another type", `[{"type":"xp_total","target":10,"item_key":"iron_ore"}]`, "c[0].item_key"},
		{"unknown field", `[{"type":"flag","flag_key":"met_elder","script":"rm -rf"}]`, "c[0].script"},
		{"non-positive target", `[{"type":"xp_total","target":0}]`, "c[0].target"},
		{"target on a flag", `[{"type":"flag","flag_key":"met_elder","target":1}]`, "c[0].target"},
		{"unknown item", `[{"type":"item_consumed","item_key":"gold","target":1}]`, "c[0].item_key"},
		{"missing currency", `[{"type":"currency_spent","target":1}]`, "c[0].currency_key"},
		{"unknown leaderboard", `[{"type":"leaderboard_score","leaderboard_key":"global","target":1}]`, "c[0].leaderboard_key"},
		{"flag used as counter", `[{"type":"stat","stat_key":"met_elder","target":1}]`, "c[0].stat_key"},
		{"counter used as flag", `[{"type":"flag","flag_key":"trolls"}]`, "c[0].flag_key"},
		{"unreachable level", `[{"type":"level_reached","target":5}]`, "c[0].target"},
		{"too many", `[{"type":"xp_total","target":1},{"type":"xp_total","target":1},{"type":"xp_total","target":1}]`, "c"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var errs platform.FieldErrors
			ValidateConditions("c", conditions(t, test.conds), 2+boolInt(test.name == "valid set"), refs(), &errs)
			if got := fieldPaths(errs); got != test.want {
				t.Fatalf("errors=%v want %q", errs, test.want)
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestValidateConditionsWithoutRefsChecksShapeOnly(t *testing.T) {
	var errs platform.FieldErrors
	ValidateConditions("c", conditions(t, `[{"type":"item_granted","item_key":"anything","target":1},{"type":"stat","stat_key":"whatever","target":2}]`), 5, nil, &errs)
	if len(errs) != 0 {
		t.Fatalf("shape-only validation checked references: %v", errs)
	}
}

func TestValidateRewards(t *testing.T) {
	var errs platform.FieldErrors
	ValidateRewards("r", Rewards{
		Items:      []ItemReward{{ItemKey: "sword", Quantity: 2}, {ItemKey: "gold", Quantity: 1}, {ItemKey: "sword", Quantity: 1}},
		Currencies: []CurrencyReward{{CurrencyKey: "credits", Amount: 0}},
		XP:         -5,
	}, refs(), &errs)
	want := "r.items[0].quantity,r.items[1].item_key,r.items[2].item_key,r.currencies[0].amount,r.xp"
	if got := fieldPaths(errs); got != want {
		t.Fatalf("errors=%v\nwant %s", got, want)
	}
}

func TestEvaluate(t *testing.T) {
	f := NewFacts()
	f.XP, f.Level = 150, 2
	f.ItemGranted["iron_ore"], f.ItemGranted[""] = 4, 9 // 9 grants of any item, 4 of them ore
	f.CurrencySpent["credits"] = 300
	f.Stats["trolls"] = 100
	f.Flags["met_elder"] = true
	f.Scores["speedrun"] = Score{Direction: "lower", Best: 58}
	f.Scores["global"] = Score{Direction: "higher", Best: 900}

	got := Evaluate(conditions(t, `[
		{"type":"xp_total","target":100},
		{"type":"level_reached","target":3},
		{"type":"item_granted","item_key":"iron_ore","target":5},
		{"type":"item_granted","target":5},
		{"type":"currency_spent","currency_key":"credits","target":250},
		{"type":"currency_earned","currency_key":"credits","target":1},
		{"type":"stat","stat_key":"trolls","target":100},
		{"type":"flag","flag_key":"met_elder"},
		{"type":"flag","flag_key":"entered_mine"},
		{"type":"leaderboard_score","leaderboard_key":"speedrun","target":60},
		{"type":"leaderboard_score","leaderboard_key":"global","target":1000},
		{"type":"leaderboard_score","leaderboard_key":"weekly","target":1}
	]`), f)
	wantMet := []bool{true, false, false, true, true, false, true, true, false, true, false, false}
	wantCurrent := []int64{150, 2, 4, 9, 300, 0, 100, 1, 0, 58, 900, 0}
	for i, p := range got {
		if p.Met != wantMet[i] || p.Current != wantCurrent[i] {
			t.Errorf("condition %d (%s): met=%v current=%d, want met=%v current=%d", i, p.Type, p.Met, p.Current, wantMet[i], wantCurrent[i])
		}
	}
	if got[11].HasValue || !got[9].HasValue {
		t.Error("has_value should be false only for a board with no score")
	}
	if got[7].Target != 1 {
		t.Errorf("flag target=%d, want 1", got[7].Target)
	}
}

func TestAllMet(t *testing.T) {
	if AllMet(nil) {
		t.Fatal("no conditions must not count as met")
	}
	if AllMet([]Progress{{Met: true}, {Met: false}}) || !AllMet([]Progress{{Met: true}, {Met: true}}) {
		t.Fatal("AllMet is wrong")
	}
}

func TestConditionRoundTripOmitsUnusedFields(t *testing.T) {
	b, _ := json.Marshal(conditions(t, `[{"type":"flag","flag_key":"met_elder"}]`))
	if string(b) != `[{"type":"flag","flag_key":"met_elder"}]` {
		t.Fatalf("marshal=%s", b)
	}
}
