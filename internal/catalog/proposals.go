package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// This file is what other packages (the AI assistant) need to propose configuration
// without bypassing catalog's rules: read and lock the draft, merge a set of
// definitions into it, and check a candidate draft exactly as publish would.

// CheckConfig decodes raw and returns every field error publish would report: the
// whole-configuration rules plus the rules that depend on recorded history. An empty
// slice means the configuration is publishable as of now.
func CheckConfig(ctx context.Context, q platform.Querier, projectID, envID string, raw []byte) (Config, []platform.FieldError, error) {
	cfg, errs := DecodeConfig(raw)
	if errs != nil {
		return cfg, errs, nil
	}
	errs = ValidateConfig(cfg)
	changeErrs, err := changeErrors(ctx, q, projectID, envID, cfg)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, append(errs, changeErrs...), nil
}

// ReadDraft returns the environment's draft (normalized: every collection present).
func ReadDraft(ctx context.Context, q platform.Querier, projectID, envID string) ([]byte, error) {
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
		return nil, err
	}
	return normalized(raw), nil
}

// LockDraft reads the draft and locks its row for the rest of tx, like editDraft.
func LockDraft(ctx context.Context, tx pgx.Tx, projectID, envID string) ([]byte, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2 FOR UPDATE`, projectID, envID).Scan(&raw); err != nil {
		return nil, err
	}
	return normalized(raw), nil
}

// WriteDraft replaces the draft inside tx (the caller holds the lock from LockDraft).
func WriteDraft(ctx context.Context, tx pgx.Tx, projectID, envID string, raw []byte) error {
	_, err := tx.Exec(ctx, `UPDATE configuration_drafts SET config=$3,updated_at=now() WHERE project_id=$1 AND environment_id=$2`, projectID, envID, raw)
	return err
}

func normalized(raw []byte) []byte {
	draft, ok := normalizeDraft(raw)
	if !ok {
		draft, _ = normalizeDraft([]byte("{}"))
	}
	b, _ := json.Marshal(draft)
	return b
}

// Proposal is a set of definitions to merge into a draft. Empty collections are left
// alone; a non-empty Progression replaces the curve.
type Proposal struct {
	Items        []ItemConfig        `json:"items,omitempty"`
	Currencies   []CurrencyConfig    `json:"currencies,omitempty"`
	Progression  []int64             `json:"progression,omitempty"`
	Stats        []StatConfig        `json:"stats,omitempty"`
	Achievements []AchievementConfig `json:"achievements,omitempty"`
	Quests       []QuestConfig       `json:"quests,omitempty"`
}

// Empty reports whether the proposal changes nothing.
func (p Proposal) Empty() bool {
	return len(p.Items)+len(p.Currencies)+len(p.Progression)+len(p.Stats)+len(p.Achievements)+len(p.Quests) == 0
}

// MergeProposal returns draftRaw with p merged in, keeping every other key (such as
// editor_layout) as it is. A definition whose key already exists replaces the existing
// one in place when replace is true; otherwise it is reported as a conflict at
// "proposal.<collection>[i].key" and the whole merge should be treated as invalid,
// because silently skipping it would leave references to a definition the person
// never saw.
func MergeProposal(draftRaw []byte, p Proposal, replace bool) ([]byte, []platform.FieldError, error) {
	draft, ok := normalizeDraft(draftRaw)
	if !ok {
		return nil, nil, fmt.Errorf("the draft is not a JSON object")
	}
	var conflicts platform.FieldErrors
	merge := func(collection string, defs []any, keyOf func(int) string) {
		if len(defs) == 0 {
			return
		}
		entries, keys := collectionEntries(draft, collection)
		for i, def := range defs {
			b, _ := json.Marshal(def)
			key := keyOf(i)
			if at := slices.Index(keys, key); at >= 0 {
				if !replace {
					conflicts.Add(fmt.Sprintf("proposal.%s[%d].key", collection, i), "%q already exists in the draft; choose replace to overwrite it", key)
					continue
				}
				entries[at] = b
				continue
			}
			entries = append(entries, b)
			keys = append(keys, key)
		}
		setCollection(draft, collection, entries)
	}
	merge("items", toAny(p.Items), func(i int) string { return p.Items[i].Key })
	merge("currencies", toAny(p.Currencies), func(i int) string { return p.Currencies[i].Key })
	merge("stats", toAny(p.Stats), func(i int) string { return p.Stats[i].Key })
	merge("achievements", toAny(p.Achievements), func(i int) string { return p.Achievements[i].Key })
	merge("quests", toAny(p.Quests), func(i int) string { return p.Quests[i].Key })
	if len(p.Progression) > 0 {
		draft["progression"], _ = json.Marshal(p.Progression)
	}
	b, err := json.Marshal(draft)
	return b, conflicts, err
}

func toAny[T any](items []T) []any {
	out := make([]any, len(items))
	for i := range items {
		out[i] = items[i]
	}
	return out
}
