package catalog_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/demo"
	"realmstack-backend/internal/goals"
	"realmstack-backend/internal/platform"
)

// fields returns the field paths of validation errors.
func fields(errs []platform.FieldError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Field
	}
	return out
}

func TestValidateConfigReturnsFieldPaths(t *testing.T) {
	if errs := catalog.ValidateConfig(demo.Configuration()); len(errs) != 0 {
		t.Fatalf("demo config rejected: %v", errs)
	}
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		change func(*catalog.Config)
		want   []string
	}{
		{"non-increasing thresholds", func(c *catalog.Config) { c.Progression = []int64{100, 100} }, []string{"progression[1]"}},
		{"zero threshold", func(c *catalog.Config) { c.Progression = []int64{0, 100} }, []string{"progression[0]"}},
		// Without a curve, level 3 (needed by the demo "veteran" achievement) is unreachable.
		{"empty progression", func(c *catalog.Config) { c.Progression = nil }, []string{"progression", "achievements[1].conditions[0].target"}},
		{"duplicate item", func(c *catalog.Config) { c.Items = append(c.Items, c.Items[0]) }, []string{"items[2].key"}},
		{"non-string slot", func(c *catalog.Config) {
			c.Items = append(c.Items, catalog.ItemConfig{Key: "bad_slot", Name: "Bad", MaxStack: 1, Metadata: map[string]any{"slot": 5}})
		}, []string{"items[2].metadata.slot"}},
		{"non-stackable stack size", func(c *catalog.Config) { c.Items[1].MaxStack = 5 }, []string{"items[1].max_stack"}},
		// Replacing "gems" also breaks every achievement and quest reward that pays gems.
		{"bad currency", func(c *catalog.Config) { c.Currencies[1] = catalog.CurrencyConfig{Key: "Bad Key"} }, []string{"currencies[1].key", "currencies[1].name",
			"achievements[1].rewards.currencies[0].currency_key", "quests[1].rewards.currencies[0].currency_key",
			"quests[2].rewards.currencies[0].currency_key", "quests[4].rewards.currencies[0].currency_key"}},
		{"stat kind mismatch", func(c *catalog.Config) { c.Stats[1].Kind = "counter" }, []string{"achievements[3].conditions[0].flag_key", "quests[0].stages[0].conditions[0].flag_key"}},
		{"bad stat and achievement", func(c *catalog.Config) {
			c.Stats = append(c.Stats, catalog.StatConfig{Key: "x", Name: "X", Kind: "gauge"})
			c.Achievements[0].ClaimableBy = "anyone"
		}, []string{"stats[4].kind", "achievements[0].claimable_by"}},
		{"duplicate leaderboard", func(c *catalog.Config) { c.Leaderboards = append(c.Leaderboards, c.Leaderboards[0]) }, []string{"leaderboards[1].key"}},
		{"bad direction and season", func(c *catalog.Config) {
			c.Leaderboards[0].Direction = "sideways"
			c.Leaderboards[0].SeasonStart, c.Leaderboards[0].SeasonEnd = &start, &start
		}, []string{"leaderboards[0].direction", "leaderboards[0].season_end"}},
		{"several problems at once", func(c *catalog.Config) {
			c.Progression = []int64{5, 1}
			c.Items[0].Name = ""
		}, []string{"items[0].name", "progression[1]"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := demo.Configuration()
			test.change(&cfg)
			if got := fields(catalog.ValidateConfig(cfg)); strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("fields=%v want %v", got, test.want)
			}
		})
	}
}

func TestValidateQuests(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	stage := func(key string) catalog.QuestStageConfig {
		return catalog.QuestStageConfig{Key: key, Name: key, Conditions: []goals.Condition{{Type: goals.Stat, StatKey: "enemies_defeated", Target: 1}}}
	}
	tests := []struct {
		name   string
		change func(*catalog.Config)
		want   []string
	}{
		// demo quests: 0 the_awakening, 1 into_the_mine (needs 0), 2 the_mentors_trial
		// (needs 1 and first_blood), 3 daily_hunt, 4 weekly_trader.
		{"cycle across three quests", func(c *catalog.Config) { c.Quests[0].RequiresQuests = []string{"the_mentors_trial"} },
			[]string{"quests[1].requires_quests[0]"}},
		{"two-quest cycle", func(c *catalog.Config) {
			c.Quests[3].RequiresQuests = []string{"weekly_trader"}
			c.Quests[4].RequiresQuests = []string{"daily_hunt"}
		},
			[]string{"quests[4].requires_quests[0]"}},
		{"self and unknown prerequisites", func(c *catalog.Config) {
			c.Quests[3].RequiresQuests = []string{"daily_hunt", "nope", "nope"}
			c.Quests[3].RequiresAchievements = []string{"missing"}
		}, []string{"quests[3].requires_quests[0]", "quests[3].requires_quests[1]", "quests[3].requires_quests[2]", "quests[3].requires_achievements[0]"}},
		{"no stages", func(c *catalog.Config) { c.Quests[3].Stages = nil }, []string{"quests[3].stages"}},
		{"more than 20 stages", func(c *catalog.Config) {
			c.Quests[3].Stages = nil
			for i := range 21 {
				c.Quests[3].Stages = append(c.Quests[3].Stages, stage(fmt.Sprintf("s%d", i)))
			}
		}, []string{"quests[3].stages"}},
		{"duplicate stage key", func(c *catalog.Config) { c.Quests[1].Stages[2].Key = "enter" }, []string{"quests[1].stages[2].key"}},
		{"bad stage condition and reward", func(c *catalog.Config) {
			c.Quests[1].Stages[1].Conditions[0].StatKey = "met_the_mentor"
			c.Quests[1].Stages[1].Rewards.Items[0].Quantity = 1000
		}, []string{"quests[1].stages[1].conditions[0].stat_key", "quests[1].stages[1].rewards.items[0].quantity"}},
		{"invalid window and repeat", func(c *catalog.Config) {
			c.Quests[3].AvailableFrom, c.Quests[3].AvailableUntil = &from, &from
			c.Quests[3].Repeat = "hourly"
		}, []string{"quests[3].repeat", "quests[3].available_until"}},
		{"duplicate quest key", func(c *catalog.Config) { c.Quests = append(c.Quests, c.Quests[3]) }, []string{"quests[5].key"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := demo.Configuration()
			test.change(&cfg)
			if got := fields(catalog.ValidateConfig(cfg)); strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("fields=%v want %v", got, test.want)
			}
		})
	}
	// The cycle message spells out the loop.
	cfg := demo.Configuration()
	cfg.Quests[0].RequiresQuests = []string{"the_mentors_trial"}
	errs := catalog.ValidateConfig(cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "the_awakening requires the_mentors_trial requires into_the_mine requires the_awakening") {
		t.Fatalf("errs=%v", errs)
	}
}

func TestDecodeConfigReportsShapeErrors(t *testing.T) {
	if _, errs := catalog.DecodeConfig([]byte(`{"progression":"fast"}`)); len(errs) != 1 || errs[0].Field != "progression" {
		t.Fatalf("errs=%v", errs)
	}
}
