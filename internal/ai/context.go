package ai

import (
	"encoding/json"
	"fmt"

	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/goals"
	"realmstack-backend/internal/inventory"
)

// MaxContextBytes caps the configuration sent to the provider. A larger draft is an
// error, never silently truncated: half a catalog would make the model invent
// references to definitions it could not see.
const MaxContextBytes = 200_000

// The model sees only the draft's definitions, through these allow-listed shapes
// (never send player PII, secrets, credentials or production data). The
// draft is the only input: no player, ledger, credential or user table is read when
// building a prompt. Item metadata is reduced to its slot because metadata is free-form
// and could hold anything a designer pasted into it.
type contextItem struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Category  string `json:"category,omitempty"`
	Stackable bool   `json:"stackable"`
	MaxStack  int    `json:"max_stack"`
	Slot      string `json:"slot,omitempty"`
}

type contextBoard struct {
	Key       string `json:"key"`
	Direction string `json:"direction"`
	Seasonal  bool   `json:"seasonal,omitempty"`
}

type contextAchievement struct {
	Key         string            `json:"key"`
	Name        string            `json:"name"`
	Hidden      bool              `json:"hidden,omitempty"`
	ClaimableBy string            `json:"claimable_by"`
	Conditions  []goals.Condition `json:"conditions"`
	Rewards     goals.Rewards     `json:"rewards"`
}

type contextStage struct {
	Key        string            `json:"key"`
	Name       string            `json:"name"`
	Conditions []goals.Condition `json:"conditions"`
}

type contextQuest struct {
	Key                  string         `json:"key"`
	Name                 string         `json:"name"`
	Chain                string         `json:"chain,omitempty"`
	RequiresQuests       []string       `json:"requires_quests,omitempty"`
	RequiresAchievements []string       `json:"requires_achievements,omitempty"`
	Repeat               string         `json:"repeat"`
	Stages               []contextStage `json:"stages"`
}

type configContext struct {
	Items        []contextItem            `json:"items"`
	Currencies   []catalog.CurrencyConfig `json:"currencies"`
	Progression  []int64                  `json:"progression"`
	MaxLevel     int                      `json:"max_level"`
	Leaderboards []contextBoard           `json:"leaderboards"`
	Stats        []catalog.StatConfig     `json:"stats"`
	Achievements []contextAchievement     `json:"achievements"`
	Quests       []contextQuest           `json:"quests"`
}

// buildContext renders the allow-listed view of a draft as JSON.
func buildContext(cfg catalog.Config) (string, error) {
	out := configContext{
		Items: []contextItem{}, Currencies: []catalog.CurrencyConfig{}, Progression: cfg.Progression, MaxLevel: len(cfg.Progression) + 1,
		Leaderboards: []contextBoard{}, Stats: []catalog.StatConfig{}, Achievements: []contextAchievement{}, Quests: []contextQuest{},
	}
	if out.Progression == nil {
		out.Progression = []int64{}
	}
	for _, x := range cfg.Items {
		slot, _ := inventory.Slot(x.Metadata)
		out.Items = append(out.Items, contextItem{Key: x.Key, Name: x.Name, Category: x.Category, Stackable: x.Stackable, MaxStack: x.MaxStack, Slot: slot})
	}
	for _, x := range cfg.Currencies {
		out.Currencies = append(out.Currencies, catalog.CurrencyConfig{Key: x.Key, Name: x.Name})
	}
	for _, x := range cfg.Leaderboards {
		out.Leaderboards = append(out.Leaderboards, contextBoard{Key: x.Key, Direction: x.Direction, Seasonal: x.SeasonStart != nil || x.SeasonEnd != nil})
	}
	for _, x := range cfg.Stats {
		out.Stats = append(out.Stats, catalog.StatConfig{Key: x.Key, Name: x.Name, Kind: x.Kind})
	}
	for _, x := range cfg.Achievements {
		out.Achievements = append(out.Achievements, contextAchievement{Key: x.Key, Name: x.Name, Hidden: x.Hidden, ClaimableBy: x.ClaimPolicy(), Conditions: x.Conditions, Rewards: x.Rewards})
	}
	for _, x := range cfg.Quests {
		q := contextQuest{Key: x.Key, Name: x.Name, Chain: x.Chain, RequiresQuests: x.RequiresQuests, RequiresAchievements: x.RequiresAchievements, Repeat: x.RepeatMode(), Stages: []contextStage{}}
		for _, s := range x.Stages {
			q.Stages = append(q.Stages, contextStage{Key: s.Key, Name: s.Name, Conditions: s.Conditions})
		}
		out.Quests = append(out.Quests, q)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	if len(b) > MaxContextBytes {
		return "", fmt.Errorf("the configuration is %d bytes; the assistant accepts at most %d", len(b), MaxContextBytes)
	}
	return string(b), nil
}
