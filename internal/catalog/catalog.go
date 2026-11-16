// Package catalog owns game configuration: the editable draft, per-definition
// editing, field-level validation, publishing immutable versions, and syncing the
// published definitions that runtime packages read.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/goals"
	"realmstack-backend/internal/inventory"
	"realmstack-backend/internal/platform"
)

type ItemConfig struct {
	Key       string         `json:"key"`
	Name      string         `json:"name"`
	Category  string         `json:"category"`
	Stackable bool           `json:"stackable"`
	MaxStack  int            `json:"max_stack"`
	Metadata  map[string]any `json:"metadata"`
}
type CurrencyConfig struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}
type LeaderboardConfig struct {
	Key         string     `json:"key"`
	Direction   string     `json:"direction"`
	SeasonStart *time.Time `json:"season_start"`
	SeasonEnd   *time.Time `json:"season_end"`
}

// StatConfig defines a server-reported counter or flag.
type StatConfig struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind"` // "counter" or "flag"
}

// AchievementConfig defines a permanent, passive goal.
type AchievementConfig struct {
	Key         string            `json:"key"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Hidden      bool              `json:"hidden"`
	Conditions  []goals.Condition `json:"conditions"`
	Rewards     goals.Rewards     `json:"rewards"`
	ClaimableBy string            `json:"claimable_by,omitempty"` // "player" (default) or "server"
}

// ClaimPolicy returns the effective claim policy ("player" when omitted).
func (a AchievementConfig) ClaimPolicy() string {
	if a.ClaimableBy == "" {
		return "player"
	}
	return a.ClaimableBy
}

// QuestStageConfig is one ordered step of a quest.
type QuestStageConfig struct {
	Key        string            `json:"key"`
	Name       string            `json:"name"`
	Conditions []goals.Condition `json:"conditions"`
	Rewards    goals.Rewards     `json:"rewards"`
}

// QuestConfig defines an active, staged goal (spec 2.7). One model covers story
// (chained), side, repeatable (daily/weekly) and event (windowed) quests.
type QuestConfig struct {
	Key                  string             `json:"key"`
	Name                 string             `json:"name"`
	Description          string             `json:"description"`
	Chain                string             `json:"chain,omitempty"`
	RequiresQuests       []string           `json:"requires_quests"`
	RequiresAchievements []string           `json:"requires_achievements"`
	Stages               []QuestStageConfig `json:"stages"`
	Rewards              goals.Rewards      `json:"rewards"`
	ClaimableBy          string             `json:"claimable_by,omitempty"` // "player" (default) or "server"
	AutoAccept           bool               `json:"auto_accept"`
	Repeat               string             `json:"repeat,omitempty"` // "none" (default), "daily" or "weekly"
	AvailableFrom        *time.Time         `json:"available_from"`
	AvailableUntil       *time.Time         `json:"available_until"`
}

// ClaimPolicy returns the effective claim policy ("player" when omitted).
func (q QuestConfig) ClaimPolicy() string {
	if q.ClaimableBy == "" {
		return "player"
	}
	return q.ClaimableBy
}

// RepeatMode returns the effective repeat mode ("none" when omitted).
func (q QuestConfig) RepeatMode() string {
	if q.Repeat == "" {
		return "none"
	}
	return q.Repeat
}

type Config struct {
	Items        []ItemConfig        `json:"items"`
	Currencies   []CurrencyConfig    `json:"currencies"`
	Progression  []int64             `json:"progression"`
	Leaderboards []LeaderboardConfig `json:"leaderboards"`
	Stats        []StatConfig        `json:"stats"`
	Achievements []AchievementConfig `json:"achievements"`
	Quests       []QuestConfig       `json:"quests"`
	// The draft may also carry "editor_layout" (quest node positions saved by the
	// dashboard). It is deliberately not decoded: publishing ignores it (spec 5.4).
}

// Handler serves configuration routes.
type Handler struct{ *platform.Env }

// DraftCollections are the definition arrays every draft carries.
var DraftCollections = []string{"items", "currencies", "leaderboards", "stats", "achievements", "quests"}

// Limits from the spec: 1–5 conditions per achievement or quest stage (2.6, 2.7) and
// 1–20 stages per quest.
const (
	MaxAchievementConditions = 5
	MaxStageConditions       = 5
	MaxQuestStages           = 20
)

func validateStat(path string, x StatConfig, errs *platform.FieldErrors) {
	if !platform.KeyPattern.MatchString(x.Key) {
		errs.Add(path+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
	if strings.TrimSpace(x.Name) == "" || len(x.Name) > 100 {
		errs.Add(path+".name", "is required and must be at most 100 characters")
	}
	if x.Kind != "counter" && x.Kind != "flag" {
		errs.Add(path+".kind", "must be counter or flag")
	}
}

// validateAchievement checks one achievement; refs nil skips reference checks.
func validateAchievement(path string, x AchievementConfig, refs *goals.Refs, errs *platform.FieldErrors) {
	if !platform.KeyPattern.MatchString(x.Key) {
		errs.Add(path+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
	if strings.TrimSpace(x.Name) == "" || len(x.Name) > 100 {
		errs.Add(path+".name", "is required and must be at most 100 characters")
	}
	if len(x.Description) > 500 {
		errs.Add(path+".description", "must be at most 500 characters")
	}
	if x.ClaimableBy != "" && x.ClaimableBy != "player" && x.ClaimableBy != "server" {
		errs.Add(path+".claimable_by", "must be player or server")
	}
	goals.ValidateConditions(path+".conditions", x.Conditions, MaxAchievementConditions, refs, errs)
	goals.ValidateRewards(path+".rewards", x.Rewards, refs, errs)
}

// questRefs is what quest prerequisites may reference. nil skips the checks (one
// quest edited on its own); validate and publish pass the whole draft's keys.
type questRefs struct {
	quests, achievements map[string]bool
}

// validateQuest checks one quest; nil refs and qrefs skip reference checks.
func validateQuest(path string, x QuestConfig, refs *goals.Refs, qrefs *questRefs, errs *platform.FieldErrors) {
	if !platform.KeyPattern.MatchString(x.Key) {
		errs.Add(path+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
	if strings.TrimSpace(x.Name) == "" || len(x.Name) > 100 {
		errs.Add(path+".name", "is required and must be at most 100 characters")
	}
	if len(x.Description) > 500 {
		errs.Add(path+".description", "must be at most 500 characters")
	}
	if len(x.Chain) > 64 {
		errs.Add(path+".chain", "must be at most 64 characters")
	}
	if x.ClaimableBy != "" && x.ClaimableBy != "player" && x.ClaimableBy != "server" {
		errs.Add(path+".claimable_by", "must be player or server")
	}
	switch x.Repeat {
	case "", "none", "daily", "weekly":
	default:
		errs.Add(path+".repeat", "must be none, daily or weekly")
	}
	if x.AvailableFrom != nil && x.AvailableUntil != nil && !x.AvailableFrom.Before(*x.AvailableUntil) {
		errs.Add(path+".available_until", "must be after available_from")
	}
	seen := map[string]bool{}
	for j, key := range x.RequiresQuests {
		p := fmt.Sprintf("%s.requires_quests[%d]", path, j)
		switch {
		case key == x.Key:
			errs.Add(p, "a quest cannot require itself")
		case seen[key]:
			errs.Add(p, "duplicates another required quest")
		case qrefs != nil && !qrefs.quests[key]:
			errs.Add(p, "references unknown quest %q", key)
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for j, key := range x.RequiresAchievements {
		p := fmt.Sprintf("%s.requires_achievements[%d]", path, j)
		switch {
		case seen[key]:
			errs.Add(p, "duplicates another required achievement")
		case qrefs != nil && !qrefs.achievements[key]:
			errs.Add(p, "references unknown achievement %q", key)
		}
		seen[key] = true
	}
	if len(x.Stages) < 1 || len(x.Stages) > MaxQuestStages {
		errs.Add(path+".stages", "needs 1 to %d stages", MaxQuestStages)
	}
	stageKeys := map[string]int{}
	for i, st := range x.Stages {
		p := fmt.Sprintf("%s.stages[%d]", path, i)
		if !platform.KeyPattern.MatchString(st.Key) {
			errs.Add(p+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
		} else if j, dup := stageKeys[st.Key]; dup {
			errs.Add(p+".key", "duplicates stages[%d].key", j)
		} else {
			stageKeys[st.Key] = i
		}
		if strings.TrimSpace(st.Name) == "" || len(st.Name) > 100 {
			errs.Add(p+".name", "is required and must be at most 100 characters")
		}
		goals.ValidateConditions(p+".conditions", st.Conditions, MaxStageConditions, refs, errs)
		goals.ValidateRewards(p+".rewards", st.Rewards, refs, errs)
	}
	goals.ValidateRewards(path+".rewards", x.Rewards, refs, errs)
}

// questCycleErrors reports every requires_quests edge that closes a cycle, at the
// edge's own path (quests[3].requires_quests[0]), with the cycle spelled out. Every
// quest on a cycle would stay locked forever. It is a depth-first search: an edge to
// a quest still on the stack is a back edge, i.e. a cycle.
func questCycleErrors(quests []QuestConfig, errs *platform.FieldErrors) {
	index := map[string]int{}
	for i, q := range quests {
		if _, dup := index[q.Key]; !dup {
			index[q.Key] = i
		}
	}
	const (
		unvisited = iota
		onStack
		done
	)
	state := make([]int, len(quests))
	var stack []int
	var visit func(i int)
	visit = func(i int) {
		state[i] = onStack
		stack = append(stack, i)
		for j, req := range quests[i].RequiresQuests {
			k, ok := index[req]
			if !ok || k == i { // unknown and self references are reported by validateQuest
				continue
			}
			switch state[k] {
			case unvisited:
				visit(k)
			case onStack:
				// The cycle is the stack from k up to i, closed by this edge.
				start := slices.Index(stack, k)
				names := make([]string, 0, len(stack)-start+1)
				for _, n := range stack[start:] {
					names = append(names, quests[n].Key)
				}
				names = append(names, quests[k].Key)
				errs.Add(fmt.Sprintf("quests[%d].requires_quests[%d]", i, j), "creates a prerequisite cycle: %s", strings.Join(names, " requires "))
			}
		}
		stack = stack[:len(stack)-1]
		state[i] = done
	}
	for i := range quests {
		if state[i] == unvisited {
			visit(i)
		}
	}
}

// refsFor collects what conditions and rewards in cfg may reference. Only well-formed
// keys are included, so a bad item is reported once (on the item), not again on every
// reference to it.
func refsFor(cfg Config) *goals.Refs {
	refs := &goals.Refs{Items: map[string]int64{}, Currencies: map[string]bool{}, Leaderboards: map[string]bool{}, Stats: map[string]string{}, MaxLevel: len(cfg.Progression) + 1}
	for _, x := range cfg.Items {
		refs.Items[x.Key] = int64(x.MaxStack)
	}
	for _, x := range cfg.Currencies {
		refs.Currencies[x.Key] = true
	}
	for _, x := range cfg.Leaderboards {
		refs.Leaderboards[x.Key] = true
	}
	for _, x := range cfg.Stats {
		refs.Stats[x.Key] = x.Kind
	}
	return refs
}

func validateItem(path string, x ItemConfig, errs *platform.FieldErrors) {
	if !platform.KeyPattern.MatchString(x.Key) {
		errs.Add(path+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
	if strings.TrimSpace(x.Name) == "" || len(x.Name) > 100 {
		errs.Add(path+".name", "is required and must be at most 100 characters")
	}
	if len(x.Category) > 64 {
		errs.Add(path+".category", "must be at most 64 characters")
	}
	switch {
	case x.MaxStack < 1 || x.MaxStack > 100000:
		errs.Add(path+".max_stack", "must be between 1 and 100000")
	case !x.Stackable && x.MaxStack != 1:
		errs.Add(path+".max_stack", "must be 1 for a non-stackable item")
	}
	if _, declared := x.Metadata["slot"]; declared {
		if slot, ok := inventory.Slot(x.Metadata); !ok || !platform.KeyPattern.MatchString(slot) {
			errs.Add(path+".metadata.slot", "must be a valid slot key")
		}
	}
}

func validateCurrency(path string, x CurrencyConfig, errs *platform.FieldErrors) {
	if !platform.KeyPattern.MatchString(x.Key) {
		errs.Add(path+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
	if strings.TrimSpace(x.Name) == "" || len(x.Name) > 100 {
		errs.Add(path+".name", "is required and must be at most 100 characters")
	}
}

func validateLeaderboard(path string, x LeaderboardConfig, errs *platform.FieldErrors) {
	if !platform.KeyPattern.MatchString(x.Key) {
		errs.Add(path+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
	if x.Direction != "higher" && x.Direction != "lower" {
		errs.Add(path+".direction", "must be higher or lower")
	}
	if x.SeasonStart != nil && x.SeasonEnd != nil && !x.SeasonStart.Before(*x.SeasonEnd) {
		errs.Add(path+".season_end", "must be after season_start")
	}
}

func validateProgression(path string, thresholds []int64, errs *platform.FieldErrors) {
	if len(thresholds) == 0 || len(thresholds) > 1000 {
		errs.Add(path, "needs 1 to 1000 thresholds")
	}
	for i, x := range thresholds {
		// Level 1 starts at 0 XP, so every listed threshold (levels 2+) must be positive.
		switch {
		case x <= 0:
			errs.Add(fmt.Sprintf("%s[%d]", path, i), "must be positive")
		case i > 0 && x <= thresholds[i-1]:
			errs.Add(fmt.Sprintf("%s[%d]", path, i), "must be greater than %s[%d]", path, i-1)
		}
	}
}

// ValidateConfig returns every problem in cfg as field-level errors; nil means valid.
func ValidateConfig(cfg Config) []platform.FieldError {
	var errs platform.FieldErrors
	if len(cfg.Items) > 500 {
		errs.Add("items", "at most 500 items are allowed")
	}
	if len(cfg.Currencies) > 100 {
		errs.Add("currencies", "at most 100 currencies are allowed")
	}
	if len(cfg.Leaderboards) > 100 {
		errs.Add("leaderboards", "at most 100 leaderboards are allowed")
	}
	seen := map[string]int{}
	for i, x := range cfg.Items {
		path := fmt.Sprintf("items[%d]", i)
		validateItem(path, x, &errs)
		if j, dup := seen[x.Key]; dup {
			errs.Add(path+".key", "duplicates items[%d].key", j)
		} else {
			seen[x.Key] = i
		}
	}
	seen = map[string]int{}
	for i, x := range cfg.Currencies {
		path := fmt.Sprintf("currencies[%d]", i)
		validateCurrency(path, x, &errs)
		if j, dup := seen[x.Key]; dup {
			errs.Add(path+".key", "duplicates currencies[%d].key", j)
		} else {
			seen[x.Key] = i
		}
	}
	validateProgression("progression", cfg.Progression, &errs)
	seen = map[string]int{}
	for i, x := range cfg.Leaderboards {
		path := fmt.Sprintf("leaderboards[%d]", i)
		validateLeaderboard(path, x, &errs)
		if j, dup := seen[x.Key]; dup {
			errs.Add(path+".key", "duplicates leaderboards[%d].key", j)
		} else {
			seen[x.Key] = i
		}
	}
	if len(cfg.Stats) > 500 {
		errs.Add("stats", "at most 500 stats and flags are allowed")
	}
	seen = map[string]int{}
	for i, x := range cfg.Stats {
		path := fmt.Sprintf("stats[%d]", i)
		validateStat(path, x, &errs)
		if j, dup := seen[x.Key]; dup {
			errs.Add(path+".key", "duplicates stats[%d].key", j)
		} else {
			seen[x.Key] = i
		}
	}
	if len(cfg.Achievements) > 500 {
		errs.Add("achievements", "at most 500 achievements are allowed")
	}
	refs := refsFor(cfg)
	seen = map[string]int{}
	for i, x := range cfg.Achievements {
		path := fmt.Sprintf("achievements[%d]", i)
		validateAchievement(path, x, refs, &errs)
		if j, dup := seen[x.Key]; dup {
			errs.Add(path+".key", "duplicates achievements[%d].key", j)
		} else {
			seen[x.Key] = i
		}
	}
	if len(cfg.Quests) > 500 {
		errs.Add("quests", "at most 500 quests are allowed")
	}
	qrefs := &questRefs{quests: map[string]bool{}, achievements: map[string]bool{}}
	for _, x := range cfg.Quests {
		qrefs.quests[x.Key] = true
	}
	for _, x := range cfg.Achievements {
		qrefs.achievements[x.Key] = true
	}
	seen = map[string]int{}
	for i, x := range cfg.Quests {
		path := fmt.Sprintf("quests[%d]", i)
		validateQuest(path, x, refs, qrefs, &errs)
		if j, dup := seen[x.Key]; dup {
			errs.Add(path+".key", "duplicates quests[%d].key", j)
		} else {
			seen[x.Key] = i
		}
	}
	questCycleErrors(cfg.Quests, &errs)
	return errs
}

// DecodeConfig parses a draft. A shape error (for example a string where an array is
// expected) is reported as a single field error on the document.
func DecodeConfig(raw []byte) (Config, []platform.FieldError) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		field := "configuration"
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			field = typeErr.Field
		}
		return cfg, []platform.FieldError{{Field: field, Message: "has the wrong type or format"}}
	}
	return cfg, nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// leaderboardChangeErrors rejects changes to the direction or season of a leaderboard
// that already has scores, because that would silently re-rank submitted scores.
// A new season uses a new leaderboard key.
func leaderboardChangeErrors(ctx context.Context, q platform.Querier, projectID, envID string, cfg Config) ([]platform.FieldError, error) {
	rows, err := q.Query(ctx, `SELECT d.key,d.score_direction,d.season_start,d.season_end FROM leaderboard_definitions d
		WHERE d.project_id=$1 AND d.environment_id=$2 AND EXISTS(SELECT 1 FROM score_submissions s WHERE s.leaderboard_id=d.id)`, projectID, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scored := map[string]LeaderboardConfig{}
	for rows.Next() {
		var x LeaderboardConfig
		if err = rows.Scan(&x.Key, &x.Direction, &x.SeasonStart, &x.SeasonEnd); err != nil {
			return nil, err
		}
		scored[x.Key] = x
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var errs platform.FieldErrors
	for i, x := range cfg.Leaderboards {
		old, ok := scored[x.Key]
		if !ok {
			continue
		}
		path := fmt.Sprintf("leaderboards[%d]", i)
		if x.Direction != old.Direction {
			errs.Add(path+".direction", "cannot change after scores were submitted; use a new leaderboard key")
		}
		if !sameTime(x.SeasonStart, old.SeasonStart) {
			errs.Add(path+".season_start", "cannot change after scores were submitted; use a new leaderboard key")
		}
		if !sameTime(x.SeasonEnd, old.SeasonEnd) {
			errs.Add(path+".season_end", "cannot change after scores were submitted; use a new leaderboard key")
		}
	}
	return errs, nil
}

// statChangeErrors rejects turning a counter into a flag (or back) once it has ledger
// rows: the history would no longer mean what it meant when written.
func statChangeErrors(ctx context.Context, q platform.Querier, projectID, envID string, cfg Config) ([]platform.FieldError, error) {
	rows, err := q.Query(ctx, `SELECT d.key,d.kind FROM stat_definitions d
		WHERE d.project_id=$1 AND d.environment_id=$2 AND EXISTS(SELECT 1 FROM stat_transactions t WHERE t.stat_definition_id=d.id)`, projectID, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[string]string{}
	for rows.Next() {
		var key, kind string
		if err = rows.Scan(&key, &kind); err != nil {
			return nil, err
		}
		used[key] = kind
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var errs platform.FieldErrors
	for i, x := range cfg.Stats {
		if old, ok := used[x.Key]; ok && old != x.Kind {
			errs.Add(fmt.Sprintf("stats[%d].kind", i), "cannot change from %s after values were recorded; use a new key", old)
		}
	}
	return errs, nil
}

// questChangeErrors protects the meaning of recorded quest history. Acceptances and
// claims are stored per period and per stage index, so once players have accepted or
// claimed a quest, its repeat mode cannot change (period keys would stop matching) and
// its existing stages cannot be removed or reordered (a stage claim would point at a
// different stage). New stages may be appended; anything else needs a new quest key.
func questChangeErrors(ctx context.Context, q platform.Querier, projectID, envID string, cfg Config) ([]platform.FieldError, error) {
	rows, err := q.Query(ctx, `SELECT d.key,d.repeat,d.stages FROM quest_definitions d WHERE d.project_id=$1 AND d.environment_id=$2
		AND (EXISTS(SELECT 1 FROM player_quests pq WHERE pq.quest_id=d.id) OR EXISTS(SELECT 1 FROM quest_claims qc WHERE qc.quest_id=d.id))`, projectID, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type used struct {
		repeat string
		stages []string
	}
	history := map[string]used{}
	for rows.Next() {
		var key string
		var u used
		var raw []byte
		if err = rows.Scan(&key, &u.repeat, &raw); err != nil {
			return nil, err
		}
		var stages []QuestStageConfig
		_ = json.Unmarshal(raw, &stages)
		for _, st := range stages {
			u.stages = append(u.stages, st.Key)
		}
		history[key] = u
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var errs platform.FieldErrors
	for i, x := range cfg.Quests {
		old, ok := history[x.Key]
		if !ok {
			continue
		}
		path := fmt.Sprintf("quests[%d]", i)
		if x.RepeatMode() != old.repeat {
			errs.Add(path+".repeat", "cannot change from %s after players accepted this quest; use a new quest key", old.repeat)
		}
		for j, key := range old.stages {
			if j >= len(x.Stages) || x.Stages[j].Key != key {
				errs.Add(path+".stages", "players have progress on this quest: keep its existing stages (%s) first and in order, and only append new ones", strings.Join(old.stages, ", "))
				break
			}
		}
	}
	return errs, nil
}

// changeErrors validates cfg against what is already live: rules that depend on
// recorded history rather than on the draft alone.
func changeErrors(ctx context.Context, q platform.Querier, projectID, envID string, cfg Config) ([]platform.FieldError, error) {
	board, err := leaderboardChangeErrors(ctx, q, projectID, envID, cfg)
	if err != nil {
		return nil, err
	}
	stat, err := statChangeErrors(ctx, q, projectID, envID, cfg)
	if err != nil {
		return nil, err
	}
	quest, err := questChangeErrors(ctx, q, projectID, envID, cfg)
	return slices.Concat(board, stat, quest), err
}

func invalidConfiguration(errs []platform.FieldError) error {
	return platform.Invalid("INVALID_CONFIGURATION", "The configuration is invalid.", errs)
}

// GetDraft returns the draft, with every definition array present.
func (h Handler) GetDraft(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	var raw []byte
	if err := h.DB.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
		platform.Internal(c, err)
		return
	}
	draft, ok := normalizeDraft(raw)
	if !ok {
		draft, _ = normalizeDraft([]byte("{}"))
	}
	platform.Success(c, http.StatusOK, draft)
}

// normalizeDraft parses a draft object and adds any missing definition arrays.
func normalizeDraft(raw []byte) (map[string]json.RawMessage, bool) {
	var draft map[string]json.RawMessage
	if json.Unmarshal(raw, &draft) != nil || draft == nil {
		return nil, false
	}
	for _, key := range slices.Concat(DraftCollections, []string{"progression"}) {
		if _, ok := draft[key]; !ok {
			draft[key] = json.RawMessage("[]")
		}
	}
	return draft, true
}

func (h Handler) PutDraft(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	var raw json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
		platform.BadRequest(c)
		return
	}
	draft, ok := normalizeDraft(raw)
	if !ok {
		platform.BadRequest(c)
		return
	}
	b, err := json.Marshal(draft)
	if err != nil {
		platform.BadRequest(c)
		return
	}
	_, err = h.DB.Exec(c, `INSERT INTO configuration_drafts(project_id,environment_id,config,updated_at) VALUES($1,$2,$3,now()) ON CONFLICT(project_id,environment_id) DO UPDATE SET config=EXCLUDED.config,updated_at=now()`, projectID, envID, b)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, json.RawMessage(b))
}

// ValidateDraft validates the supplied configuration body, or the stored draft when the
// body is empty, without saving anything.
func (h Handler) ValidateDraft(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	raw, err := c.GetRawData()
	if err != nil {
		platform.BadRequest(c)
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if err = h.DB.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
			platform.Internal(c, err)
			return
		}
	} else if !json.Valid(raw) {
		platform.BadRequest(c)
		return
	}
	cfg, errs := DecodeConfig(raw)
	if errs == nil {
		errs = ValidateConfig(cfg)
		changeErrs, err := changeErrors(c, h.DB, projectID, envID, cfg)
		if err != nil {
			platform.Internal(c, err)
			return
		}
		errs = append(errs, changeErrs...)
	}
	if errs == nil {
		errs = []platform.FieldError{}
	}
	platform.Success(c, http.StatusOK, gin.H{"valid": len(errs) == 0, "errors": errs})
}

func (h Handler) Publish(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	tx, err := h.DB.Begin(c)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var raw []byte
	err = tx.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2 FOR UPDATE`, projectID, envID).Scan(&raw)
	if err != nil {
		platform.NotFound(c)
		return
	}
	cfg, errs := DecodeConfig(raw)
	if errs == nil {
		errs = ValidateConfig(cfg)
		changeErrs, err := changeErrors(c, tx, projectID, envID, cfg)
		if err != nil {
			platform.Internal(c, err)
			return
		}
		errs = append(errs, changeErrs...)
	}
	if len(errs) > 0 {
		platform.Respond(c, invalidConfiguration(errs))
		return
	}
	var version int
	err = tx.QueryRow(c, `SELECT COALESCE(MAX(version),0)+1 FROM configuration_versions WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&version)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	var versionID string
	var publishedAt time.Time
	err = tx.QueryRow(c, `INSERT INTO configuration_versions(project_id,environment_id,version,config,created_by) VALUES($1,$2,$3,$4,$5) RETURNING id::text,published_at`,
		projectID, envID, version, raw, platform.GetPrincipal(c).ID).Scan(&versionID, &publishedAt)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	progression, created, err := ApplyRuntimeConfig(c, tx, projectID, envID, versionID, cfg, h.Now())
	if err != nil {
		platform.Internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusCreated, gin.H{"version": version, "published_at": publishedAt.UTC(), "progression_version": progression, "progression_version_created": created})
}

func (h Handler) ListVersions(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	rows, err := h.DB.Query(c, `SELECT id::text,version,config,published_at,COALESCE(created_by::text,'') FROM configuration_versions WHERE project_id=$1 AND environment_id=$2 ORDER BY version DESC`, projectID, envID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var id, createdBy string
		var version int
		var raw []byte
		var at time.Time
		if err = rows.Scan(&id, &version, &raw, &at, &createdBy); err != nil {
			platform.Internal(c, err)
			return
		}
		entry := gin.H{"id": id, "version": version, "config": json.RawMessage(raw), "published_at": at}
		if createdBy != "" {
			entry["published_by"] = createdBy
		}
		out = append(out, entry)
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}

// GetPublished returns the live (latest published) configuration, or
// {"version": null} when nothing has been published yet.
func (h Handler) GetPublished(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	var version int
	var raw []byte
	var at time.Time
	err := h.DB.QueryRow(c, `SELECT version,config,published_at FROM configuration_versions WHERE project_id=$1 AND environment_id=$2 ORDER BY version DESC LIMIT 1`, projectID, envID).Scan(&version, &raw, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		platform.Success(c, http.StatusOK, gin.H{"version": nil, "config": nil, "published_at": nil})
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, gin.H{"version": version, "config": json.RawMessage(raw), "published_at": at})
}

// ApplyRuntimeConfig switches the active definitions to cfg and returns the progression
// version that new players will pin to. A new progression version is created only
// when the thresholds differ from the latest one. now is the publish time: a quest
// that goes live records it as activated_at.
func ApplyRuntimeConfig(ctx context.Context, tx pgx.Tx, p, e, configVersionID string, cfg Config, now time.Time) (int, bool, error) {
	if _, err := tx.Exec(ctx, `UPDATE item_definitions SET active=false WHERE project_id=$1 AND environment_id=$2`, p, e); err != nil {
		return 0, false, err
	}
	for _, x := range cfg.Items {
		meta, _ := json.Marshal(x.Metadata)
		if x.Metadata == nil {
			meta = []byte("{}")
		}
		_, err := tx.Exec(ctx, `INSERT INTO item_definitions(project_id,environment_id,item_key,name,category,stackable,max_stack,metadata,active) VALUES($1,$2,$3,$4,$5,$6,$7,$8,true) ON CONFLICT(project_id,environment_id,item_key) DO UPDATE SET name=EXCLUDED.name,category=EXCLUDED.category,stackable=EXCLUDED.stackable,max_stack=EXCLUDED.max_stack,metadata=EXCLUDED.metadata,active=true`, p, e, x.Key, x.Name, x.Category, x.Stackable, x.MaxStack, meta)
		if err != nil {
			return 0, false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE currency_definitions SET active=false WHERE project_id=$1 AND environment_id=$2`, p, e); err != nil {
		return 0, false, err
	}
	for _, x := range cfg.Currencies {
		_, err := tx.Exec(ctx, `INSERT INTO currency_definitions(project_id,environment_id,currency_key,name,active) VALUES($1,$2,$3,$4,true) ON CONFLICT(project_id,environment_id,currency_key) DO UPDATE SET name=EXCLUDED.name,active=true`, p, e, x.Key, x.Name)
		if err != nil {
			return 0, false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE leaderboard_definitions SET active=false WHERE project_id=$1 AND environment_id=$2`, p, e); err != nil {
		return 0, false, err
	}
	for _, x := range cfg.Leaderboards {
		_, err := tx.Exec(ctx, `INSERT INTO leaderboard_definitions(project_id,environment_id,key,score_direction,season_start,season_end,active) VALUES($1,$2,$3,$4,$5,$6,true) ON CONFLICT(project_id,environment_id,key) DO UPDATE SET score_direction=EXCLUDED.score_direction,season_start=EXCLUDED.season_start,season_end=EXCLUDED.season_end,active=true`, p, e, x.Key, x.Direction, x.SeasonStart, x.SeasonEnd)
		if err != nil {
			return 0, false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE stat_definitions SET active=false WHERE project_id=$1 AND environment_id=$2`, p, e); err != nil {
		return 0, false, err
	}
	for _, x := range cfg.Stats {
		_, err := tx.Exec(ctx, `INSERT INTO stat_definitions(project_id,environment_id,key,name,kind,active) VALUES($1,$2,$3,$4,$5,true)
			ON CONFLICT(project_id,environment_id,key) DO UPDATE SET name=EXCLUDED.name,kind=EXCLUDED.kind,active=true`, p, e, x.Key, x.Name, x.Kind)
		if err != nil {
			return 0, false, err
		}
	}
	// Republishing re-evaluates progress against the new definitions (evaluation reads
	// them live). Removed achievements are deactivated, never deleted, so existing
	// claims keep pointing at them and are never revoked.
	if _, err := tx.Exec(ctx, `UPDATE achievement_definitions SET active=false WHERE project_id=$1 AND environment_id=$2`, p, e); err != nil {
		return 0, false, err
	}
	for i, x := range cfg.Achievements {
		conds, _ := json.Marshal(x.Conditions)
		rewards, _ := json.Marshal(x.Rewards)
		_, err := tx.Exec(ctx, `INSERT INTO achievement_definitions(project_id,environment_id,key,name,description,hidden,conditions,rewards,claimable_by,sort_order,active)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true)
			ON CONFLICT(project_id,environment_id,key) DO UPDATE SET name=EXCLUDED.name,description=EXCLUDED.description,hidden=EXCLUDED.hidden,
			conditions=EXCLUDED.conditions,rewards=EXCLUDED.rewards,claimable_by=EXCLUDED.claimable_by,sort_order=EXCLUDED.sort_order,active=true`,
			p, e, x.Key, x.Name, x.Description, x.Hidden, conds, rewards, x.ClaimPolicy(), i)
		if err != nil {
			return 0, false, err
		}
	}
	// Quests: deactivate only the ones that left the configuration, so a quest that
	// stays live keeps its activated_at, and one that (re)appears gets the publish time.
	questKeys := make([]string, len(cfg.Quests))
	for i, x := range cfg.Quests {
		questKeys[i] = x.Key
	}
	if _, err := tx.Exec(ctx, `UPDATE quest_definitions SET active=false WHERE project_id=$1 AND environment_id=$2 AND active AND key <> ALL($3::text[])`, p, e, questKeys); err != nil {
		return 0, false, err
	}
	for i, x := range cfg.Quests {
		stages, _ := json.Marshal(x.Stages)
		rewards, _ := json.Marshal(x.Rewards)
		reqQuests, _ := json.Marshal(nonNil(x.RequiresQuests))
		reqAchievements, _ := json.Marshal(nonNil(x.RequiresAchievements))
		var chain *string
		if x.Chain != "" {
			chain = &x.Chain
		}
		_, err := tx.Exec(ctx, `INSERT INTO quest_definitions(project_id,environment_id,key,name,description,chain,requires_quests,requires_achievements,stages,rewards,
			claimable_by,auto_accept,repeat,available_from,available_until,sort_order,active,activated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,true,$17)
			ON CONFLICT(project_id,environment_id,key) DO UPDATE SET name=EXCLUDED.name,description=EXCLUDED.description,chain=EXCLUDED.chain,
			requires_quests=EXCLUDED.requires_quests,requires_achievements=EXCLUDED.requires_achievements,stages=EXCLUDED.stages,rewards=EXCLUDED.rewards,
			claimable_by=EXCLUDED.claimable_by,auto_accept=EXCLUDED.auto_accept,repeat=EXCLUDED.repeat,available_from=EXCLUDED.available_from,
			available_until=EXCLUDED.available_until,sort_order=EXCLUDED.sort_order,
			activated_at=CASE WHEN quest_definitions.active THEN quest_definitions.activated_at ELSE EXCLUDED.activated_at END,active=true`,
			p, e, x.Key, x.Name, x.Description, chain, reqQuests, reqAchievements, stages, rewards,
			x.ClaimPolicy(), x.AutoAccept, x.RepeatMode(), x.AvailableFrom, x.AvailableUntil, i, now)
		if err != nil {
			return 0, false, err
		}
	}
	var latestRaw []byte
	var latest int
	err := tx.QueryRow(ctx, `SELECT thresholds,version FROM progression_versions WHERE project_id=$1 AND environment_id=$2 ORDER BY version DESC LIMIT 1`, p, e).Scan(&latestRaw, &latest)
	switch {
	case err == nil:
		var thresholds []int64
		if json.Unmarshal(latestRaw, &thresholds) == nil && slices.Equal(thresholds, cfg.Progression) {
			return latest, false, nil
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, false, err
	}
	thresholds, _ := json.Marshal(cfg.Progression)
	_, err = tx.Exec(ctx, `INSERT INTO progression_versions(project_id,environment_id,version,thresholds,configuration_version_id) VALUES($1,$2,$3,$4,$5)`, p, e, latest+1, thresholds, configVersionID)
	return latest + 1, err == nil, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// --- Per-definition draft editing (spec 5.2) ---

// draftEdit changes a parsed draft in place and returns the response, or an error to
// send instead (in which case nothing is saved).
type draftEdit func(draft map[string]json.RawMessage) (status int, result any, fail *platform.Error)

// editDraft applies edit to the environment's draft under a row lock.
func (h Handler) editDraft(c *gin.Context, edit draftEdit) {
	projectID, envID := platform.Owned(c)
	tx, err := h.DB.Begin(c)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var raw []byte
	if err = tx.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2 FOR UPDATE`, projectID, envID).Scan(&raw); err != nil {
		platform.Internal(c, err)
		return
	}
	draft, ok := normalizeDraft(raw)
	if !ok {
		draft, _ = normalizeDraft([]byte("{}"))
	}
	status, result, fail := edit(draft)
	if fail != nil {
		platform.FailDetails(c, fail.Status, fail.Code, fail.Message, fail.Details)
		return
	}
	b, err := json.Marshal(draft)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	if _, err = tx.Exec(c, `UPDATE configuration_drafts SET config=$3,updated_at=now() WHERE project_id=$1 AND environment_id=$2`, projectID, envID, b); err != nil {
		platform.Internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, status, result)
}

// DefinitionKind describes one draft collection: how to decode and validate an entry.
type DefinitionKind struct {
	collection, singular string
	// decode strictly parses one definition and returns it normalized, with its key.
	decode func(raw []byte) (any, string, error)
	// validate reports problems for one definition at path.
	validate func(path string, def any, errs *platform.FieldErrors)
}

func strictDecode[T any](raw []byte) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, err
	}
	if dec.More() {
		return v, errors.New("trailing data")
	}
	return v, nil
}

var DefinitionKinds = map[string]DefinitionKind{
	"items": {
		collection: "items",
		singular:   "item",
		decode: func(raw []byte) (any, string, error) {
			x, err := strictDecode[ItemConfig](raw)
			return x, x.Key, err
		},
		validate: func(path string, def any, errs *platform.FieldErrors) { validateItem(path, def.(ItemConfig), errs) },
	},
	"currencies": {
		collection: "currencies",
		singular:   "currency",
		decode: func(raw []byte) (any, string, error) {
			x, err := strictDecode[CurrencyConfig](raw)
			return x, x.Key, err
		},
		validate: func(path string, def any, errs *platform.FieldErrors) {
			validateCurrency(path, def.(CurrencyConfig), errs)
		},
	},
	"leaderboards": {
		collection: "leaderboards",
		singular:   "leaderboard",
		decode: func(raw []byte) (any, string, error) {
			x, err := strictDecode[LeaderboardConfig](raw)
			return x, x.Key, err
		},
		validate: func(path string, def any, errs *platform.FieldErrors) {
			validateLeaderboard(path, def.(LeaderboardConfig), errs)
		},
	},
	"stats": {
		collection: "stats",
		singular:   "stat",
		decode: func(raw []byte) (any, string, error) {
			x, err := strictDecode[StatConfig](raw)
			return x, x.Key, err
		},
		validate: func(path string, def any, errs *platform.FieldErrors) { validateStat(path, def.(StatConfig), errs) },
	},
	// Editing one achievement checks its shape only; references to items, stats and
	// so on are checked against the whole draft by validate and publish.
	"achievements": {
		collection: "achievements",
		singular:   "achievement",
		decode: func(raw []byte) (any, string, error) {
			x, err := strictDecode[AchievementConfig](raw)
			return x, x.Key, err
		},
		validate: func(path string, def any, errs *platform.FieldErrors) {
			validateAchievement(path, def.(AchievementConfig), nil, errs)
		},
	},
	// Like achievements, one quest is shape-checked on its own; prerequisites, cycles
	// and references are checked against the whole draft by validate and publish.
	"quests": {
		collection: "quests",
		singular:   "quest",
		decode: func(raw []byte) (any, string, error) {
			x, err := strictDecode[QuestConfig](raw)
			return x, x.Key, err
		},
		validate: func(path string, def any, errs *platform.FieldErrors) {
			validateQuest(path, def.(QuestConfig), nil, nil, errs)
		},
	},
}

// collectionEntries splits a draft collection into raw entries and their keys.
func collectionEntries(draft map[string]json.RawMessage, collection string) ([]json.RawMessage, []string) {
	var entries []json.RawMessage
	_ = json.Unmarshal(draft[collection], &entries)
	keys := make([]string, len(entries))
	for i, entry := range entries {
		var k struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(entry, &k)
		keys[i] = k.Key
	}
	return entries, keys
}

func setCollection(draft map[string]json.RawMessage, collection string, entries []json.RawMessage) {
	if entries == nil {
		entries = []json.RawMessage{}
	}
	b, _ := json.Marshal(entries)
	draft[collection] = b
}

var errDefinitionNotFound = &platform.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "The definition was not found in the draft."}

// readDefinition decodes and validates a request body for kind; pathKey, when set, is
// the key from the URL, which the body must match or omit.
func readDefinition(c *gin.Context, kind DefinitionKind, pathKey string) (json.RawMessage, string, *platform.Error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, "", &platform.Error{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Message: "The request body or fields are invalid."}
	}
	if pathKey != "" {
		// Fill in the key from the URL when the body omits it.
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) == nil && obj != nil {
			if _, has := obj["key"]; !has {
				obj["key"], _ = json.Marshal(pathKey)
				raw, _ = json.Marshal(obj)
			}
		}
	}
	def, key, err := kind.decode(raw)
	if err != nil {
		return nil, "", &platform.Error{Status: http.StatusBadRequest, Code: "INVALID_REQUEST", Message: "The definition is not valid JSON for this schema: " + err.Error() + "."}
	}
	if pathKey != "" && key != pathKey {
		return nil, "", &platform.Error{Status: http.StatusUnprocessableEntity, Code: "VALIDATION_ERROR", Message: "The body key must match the URL key.", Details: gin.H{"errors": []platform.FieldError{{Field: "key", Message: "must match the URL key"}}}}
	}
	var errs platform.FieldErrors
	kind.validate(kind.singular, def, &errs)
	if len(errs) > 0 {
		return nil, "", &platform.Error{Status: http.StatusUnprocessableEntity, Code: "VALIDATION_ERROR", Message: "The definition is invalid.", Details: gin.H{"errors": errs}}
	}
	normalized, _ := json.Marshal(def)
	return normalized, key, nil
}

func (h Handler) ListDefinitions(kind DefinitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, envID := platform.Owned(c)
		var raw []byte
		if err := h.DB.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
			platform.Internal(c, err)
			return
		}
		draft, _ := normalizeDraft(raw)
		entries, _ := collectionEntries(draft, kind.collection)
		if entries == nil {
			entries = []json.RawMessage{}
		}
		platform.Success(c, http.StatusOK, entries)
	}
}

func (h Handler) GetDefinition(kind DefinitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, envID := platform.Owned(c)
		var raw []byte
		if err := h.DB.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
			platform.Internal(c, err)
			return
		}
		draft, _ := normalizeDraft(raw)
		entries, keys := collectionEntries(draft, kind.collection)
		if i := slices.Index(keys, c.Param("key")); i >= 0 {
			platform.Success(c, http.StatusOK, entries[i])
			return
		}
		platform.Fail(c, errDefinitionNotFound.Status, errDefinitionNotFound.Code, errDefinitionNotFound.Message)
	}
}

func (h Handler) CreateDefinition(kind DefinitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		def, key, fail := readDefinition(c, kind, "")
		if fail != nil {
			platform.FailDetails(c, fail.Status, fail.Code, fail.Message, fail.Details)
			return
		}
		h.editDraft(c, func(draft map[string]json.RawMessage) (int, any, *platform.Error) {
			entries, keys := collectionEntries(draft, kind.collection)
			if slices.Contains(keys, key) {
				return 0, nil, &platform.Error{Status: http.StatusConflict, Code: "CONFLICT", Message: fmt.Sprintf("A definition with key %q already exists in the draft.", key)}
			}
			setCollection(draft, kind.collection, append(entries, def))
			return http.StatusCreated, def, nil
		})
	}
}

func (h Handler) ReplaceDefinition(kind DefinitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		def, key, fail := readDefinition(c, kind, c.Param("key"))
		if fail != nil {
			platform.FailDetails(c, fail.Status, fail.Code, fail.Message, fail.Details)
			return
		}
		h.editDraft(c, func(draft map[string]json.RawMessage) (int, any, *platform.Error) {
			entries, keys := collectionEntries(draft, kind.collection)
			i := slices.Index(keys, key)
			if i < 0 {
				return 0, nil, errDefinitionNotFound
			}
			entries[i] = def
			setCollection(draft, kind.collection, entries)
			return http.StatusOK, def, nil
		})
	}
}

func (h Handler) DeleteDefinition(kind DefinitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Param("key")
		h.editDraft(c, func(draft map[string]json.RawMessage) (int, any, *platform.Error) {
			entries, keys := collectionEntries(draft, kind.collection)
			i := slices.Index(keys, key)
			if i < 0 {
				return 0, nil, errDefinitionNotFound
			}
			setCollection(draft, kind.collection, slices.Delete(entries, i, i+1))
			return http.StatusOK, gin.H{"deleted": true, "key": key}, nil
		})
	}
}

func (h Handler) GetProgression(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	var raw []byte
	if err := h.DB.QueryRow(c, `SELECT config->'progression' FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
		platform.Internal(c, err)
		return
	}
	var thresholds []int64
	_ = json.Unmarshal(raw, &thresholds)
	if thresholds == nil {
		thresholds = []int64{}
	}
	platform.Success(c, http.StatusOK, gin.H{"thresholds": thresholds})
}

func (h Handler) PutProgression(c *gin.Context) {
	var in struct {
		Thresholds []int64 `json:"thresholds"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	var errs platform.FieldErrors
	validateProgression("thresholds", in.Thresholds, &errs)
	if len(errs) > 0 {
		platform.FailDetails(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "The progression thresholds are invalid.", gin.H{"errors": errs})
		return
	}
	h.editDraft(c, func(draft map[string]json.RawMessage) (int, any, *platform.Error) {
		draft["progression"], _ = json.Marshal(in.Thresholds)
		return http.StatusOK, gin.H{"thresholds": in.Thresholds}, nil
	})
}
