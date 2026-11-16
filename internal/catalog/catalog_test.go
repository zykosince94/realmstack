package catalog_test

import (
	"strings"
	"testing"
	"time"

	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/demo"
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
		// Replacing "gems" also breaks the achievement reward that pays gems.
		{"bad currency", func(c *catalog.Config) { c.Currencies[1] = catalog.CurrencyConfig{Key: "Bad Key"} }, []string{"currencies[1].key", "currencies[1].name", "achievements[1].rewards.currencies[0].currency_key"}},
		{"stat kind mismatch", func(c *catalog.Config) { c.Stats[1].Kind = "counter" }, []string{"achievements[3].conditions[0].flag_key"}},
		{"bad stat and achievement", func(c *catalog.Config) {
			c.Stats = append(c.Stats, catalog.StatConfig{Key: "x", Name: "X", Kind: "gauge"})
			c.Achievements[0].ClaimableBy = "anyone"
		}, []string{"stats[2].kind", "achievements[0].claimable_by"}},
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

func TestDecodeConfigReportsShapeErrors(t *testing.T) {
	if _, errs := catalog.DecodeConfig([]byte(`{"progression":"fast"}`)); len(errs) != 1 || errs[0].Field != "progression" {
		t.Fatalf("errs=%v", errs)
	}
}
