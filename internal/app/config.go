package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

type itemConfig struct {
	Key       string         `json:"key"`
	Name      string         `json:"name"`
	Category  string         `json:"category"`
	Stackable bool           `json:"stackable"`
	MaxStack  int            `json:"max_stack"`
	Metadata  map[string]any `json:"metadata"`
}
type currencyConfig struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}
type leaderboardConfig struct {
	Key         string     `json:"key"`
	Direction   string     `json:"direction"`
	SeasonStart *time.Time `json:"season_start"`
	SeasonEnd   *time.Time `json:"season_end"`
}
type runtimeConfig struct {
	Items        []itemConfig        `json:"items"`
	Currencies   []currencyConfig    `json:"currencies"`
	Progression  []int64             `json:"progression"`
	Leaderboards []leaderboardConfig `json:"leaderboards"`
}

// draftCollections are the definition arrays every draft carries.
var draftCollections = []string{"items", "currencies", "leaderboards"}

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// FieldError is one validation problem, addressed by a JSON path such as items[2].max_stack.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type fieldErrors []FieldError

func (e *fieldErrors) add(field, format string, args ...any) {
	*e = append(*e, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// itemSlot returns the equipment slot declared in an item's metadata ("slot"), if any.
func itemSlot(metadata map[string]any) (string, bool) {
	v, ok := metadata["slot"]
	if !ok {
		return "", false
	}
	slot, ok := v.(string)
	return slot, ok
}

func validateItem(path string, x itemConfig, errs *fieldErrors) {
	if !keyPattern.MatchString(x.Key) {
		errs.add(path+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
	if strings.TrimSpace(x.Name) == "" || len(x.Name) > 100 {
		errs.add(path+".name", "is required and must be at most 100 characters")
	}
	if len(x.Category) > 64 {
		errs.add(path+".category", "must be at most 64 characters")
	}
	switch {
	case x.MaxStack < 1 || x.MaxStack > 100000:
		errs.add(path+".max_stack", "must be between 1 and 100000")
	case !x.Stackable && x.MaxStack != 1:
		errs.add(path+".max_stack", "must be 1 for a non-stackable item")
	}
	if _, declared := x.Metadata["slot"]; declared {
		if slot, ok := itemSlot(x.Metadata); !ok || !keyPattern.MatchString(slot) {
			errs.add(path+".metadata.slot", "must be a valid slot key")
		}
	}
}

func validateCurrency(path string, x currencyConfig, errs *fieldErrors) {
	if !keyPattern.MatchString(x.Key) {
		errs.add(path+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
	if strings.TrimSpace(x.Name) == "" || len(x.Name) > 100 {
		errs.add(path+".name", "is required and must be at most 100 characters")
	}
}

func validateLeaderboard(path string, x leaderboardConfig, errs *fieldErrors) {
	if !keyPattern.MatchString(x.Key) {
		errs.add(path+".key", "must be 1-63 lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
	if x.Direction != "higher" && x.Direction != "lower" {
		errs.add(path+".direction", "must be higher or lower")
	}
	if x.SeasonStart != nil && x.SeasonEnd != nil && !x.SeasonStart.Before(*x.SeasonEnd) {
		errs.add(path+".season_end", "must be after season_start")
	}
}

func validateProgression(path string, thresholds []int64, errs *fieldErrors) {
	if len(thresholds) == 0 || len(thresholds) > 1000 {
		errs.add(path, "needs 1 to 1000 thresholds")
	}
	for i, x := range thresholds {
		// Level 1 starts at 0 XP, so every listed threshold (levels 2+) must be positive.
		switch {
		case x <= 0:
			errs.add(fmt.Sprintf("%s[%d]", path, i), "must be positive")
		case i > 0 && x <= thresholds[i-1]:
			errs.add(fmt.Sprintf("%s[%d]", path, i), "must be greater than %s[%d]", path, i-1)
		}
	}
}

// validateConfig returns every problem in cfg as field-level errors; nil means valid.
func validateConfig(cfg runtimeConfig) []FieldError {
	var errs fieldErrors
	if len(cfg.Items) > 500 {
		errs.add("items", "at most 500 items are allowed")
	}
	if len(cfg.Currencies) > 100 {
		errs.add("currencies", "at most 100 currencies are allowed")
	}
	if len(cfg.Leaderboards) > 100 {
		errs.add("leaderboards", "at most 100 leaderboards are allowed")
	}
	seen := map[string]int{}
	for i, x := range cfg.Items {
		path := fmt.Sprintf("items[%d]", i)
		validateItem(path, x, &errs)
		if j, dup := seen[x.Key]; dup {
			errs.add(path+".key", "duplicates items[%d].key", j)
		} else {
			seen[x.Key] = i
		}
	}
	seen = map[string]int{}
	for i, x := range cfg.Currencies {
		path := fmt.Sprintf("currencies[%d]", i)
		validateCurrency(path, x, &errs)
		if j, dup := seen[x.Key]; dup {
			errs.add(path+".key", "duplicates currencies[%d].key", j)
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
			errs.add(path+".key", "duplicates leaderboards[%d].key", j)
		} else {
			seen[x.Key] = i
		}
	}
	return errs
}

// decodeConfig parses a draft. A shape error (for example a string where an array is
// expected) is reported as a single field error on the document.
func decodeConfig(raw []byte) (runtimeConfig, []FieldError) {
	var cfg runtimeConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		field := "configuration"
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			field = typeErr.Field
		}
		return cfg, []FieldError{{Field: field, Message: "has the wrong type or format"}}
	}
	return cfg, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
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
func leaderboardChangeErrors(ctx context.Context, q querier, projectID, envID string, cfg runtimeConfig) ([]FieldError, error) {
	rows, err := q.Query(ctx, `SELECT d.key,d.score_direction,d.season_start,d.season_end FROM leaderboard_definitions d
		WHERE d.project_id=$1 AND d.environment_id=$2 AND EXISTS(SELECT 1 FROM score_submissions s WHERE s.leaderboard_id=d.id)`, projectID, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scored := map[string]leaderboardConfig{}
	for rows.Next() {
		var x leaderboardConfig
		if err = rows.Scan(&x.Key, &x.Direction, &x.SeasonStart, &x.SeasonEnd); err != nil {
			return nil, err
		}
		scored[x.Key] = x
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var errs fieldErrors
	for i, x := range cfg.Leaderboards {
		old, ok := scored[x.Key]
		if !ok {
			continue
		}
		path := fmt.Sprintf("leaderboards[%d]", i)
		if x.Direction != old.Direction {
			errs.add(path+".direction", "cannot change after scores were submitted; use a new leaderboard key")
		}
		if !sameTime(x.SeasonStart, old.SeasonStart) {
			errs.add(path+".season_start", "cannot change after scores were submitted; use a new leaderboard key")
		}
		if !sameTime(x.SeasonEnd, old.SeasonEnd) {
			errs.add(path+".season_end", "cannot change after scores were submitted; use a new leaderboard key")
		}
	}
	return errs, nil
}

func invalidConfiguration(c *gin.Context, errs []FieldError) {
	failureDetails(c, http.StatusUnprocessableEntity, "INVALID_CONFIGURATION", "The configuration is invalid.", gin.H{"errors": errs})
}

func (s *Server) getDraft(c *gin.Context) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	var config json.RawMessage
	if err := s.db.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&config); err != nil {
		internal(c, err)
		return
	}
	success(c, http.StatusOK, config)
}

// normalizeDraft parses a draft object and adds any missing definition arrays.
func normalizeDraft(raw []byte) (map[string]json.RawMessage, bool) {
	var draft map[string]json.RawMessage
	if json.Unmarshal(raw, &draft) != nil || draft == nil {
		return nil, false
	}
	for _, key := range append(draftCollections, "progression") {
		if _, ok := draft[key]; !ok {
			draft[key] = json.RawMessage("[]")
		}
	}
	return draft, true
}

func (s *Server) putDraft(c *gin.Context) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	var raw json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
		badRequest(c)
		return
	}
	draft, ok := normalizeDraft(raw)
	if !ok {
		badRequest(c)
		return
	}
	b, err := json.Marshal(draft)
	if err != nil {
		badRequest(c)
		return
	}
	_, err = s.db.Exec(c, `INSERT INTO configuration_drafts(project_id,environment_id,config,updated_at) VALUES($1,$2,$3,now()) ON CONFLICT(project_id,environment_id) DO UPDATE SET config=EXCLUDED.config,updated_at=now()`, projectID, envID, b)
	if err != nil {
		internal(c, err)
		return
	}
	success(c, http.StatusOK, json.RawMessage(b))
}

// validateDraft validates the supplied configuration body, or the stored draft when the
// body is empty, without saving anything.
func (s *Server) validateDraft(c *gin.Context) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	raw, err := c.GetRawData()
	if err != nil {
		badRequest(c)
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if err = s.db.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
			internal(c, err)
			return
		}
	} else if !json.Valid(raw) {
		badRequest(c)
		return
	}
	cfg, errs := decodeConfig(raw)
	if errs == nil {
		errs = validateConfig(cfg)
		changeErrs, err := leaderboardChangeErrors(c, s.db, projectID, envID, cfg)
		if err != nil {
			internal(c, err)
			return
		}
		errs = append(errs, changeErrs...)
	}
	if errs == nil {
		errs = []FieldError{}
	}
	success(c, http.StatusOK, gin.H{"valid": len(errs) == 0, "errors": errs})
}

func (s *Server) publishConfig(c *gin.Context) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var raw []byte
	err = tx.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2 FOR UPDATE`, projectID, envID).Scan(&raw)
	if err != nil {
		notFound(c)
		return
	}
	cfg, errs := decodeConfig(raw)
	if errs == nil {
		errs = validateConfig(cfg)
		changeErrs, err := leaderboardChangeErrors(c, tx, projectID, envID, cfg)
		if err != nil {
			internal(c, err)
			return
		}
		errs = append(errs, changeErrs...)
	}
	if len(errs) > 0 {
		invalidConfiguration(c, errs)
		return
	}
	var version int
	err = tx.QueryRow(c, `SELECT COALESCE(MAX(version),0)+1 FROM configuration_versions WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&version)
	if err != nil {
		internal(c, err)
		return
	}
	var versionID string
	var publishedAt time.Time
	err = tx.QueryRow(c, `INSERT INTO configuration_versions(project_id,environment_id,version,config,created_by) VALUES($1,$2,$3,$4,$5) RETURNING id::text,published_at`,
		projectID, envID, version, raw, getPrincipal(c).ID).Scan(&versionID, &publishedAt)
	if err != nil {
		internal(c, err)
		return
	}
	progression, created, err := applyRuntimeConfig(c, tx, projectID, envID, versionID, cfg)
	if err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, http.StatusCreated, gin.H{"version": version, "published_at": publishedAt.UTC(), "progression_version": progression, "progression_version_created": created})
}

func (s *Server) listConfigVersions(c *gin.Context) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	rows, err := s.db.Query(c, `SELECT id::text,version,config,published_at,COALESCE(created_by::text,'') FROM configuration_versions WHERE project_id=$1 AND environment_id=$2 ORDER BY version DESC`, projectID, envID)
	if err != nil {
		internal(c, err)
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
			internal(c, err)
			return
		}
		entry := gin.H{"id": id, "version": version, "config": json.RawMessage(raw), "published_at": at}
		if createdBy != "" {
			entry["published_by"] = createdBy
		}
		out = append(out, entry)
	}
	if err = rows.Err(); err != nil {
		internal(c, err)
		return
	}
	success(c, http.StatusOK, out)
}

// getPublishedConfig returns the live (latest published) configuration, or
// {"version": null} when nothing has been published yet.
func (s *Server) getPublishedConfig(c *gin.Context) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	var version int
	var raw []byte
	var at time.Time
	err := s.db.QueryRow(c, `SELECT version,config,published_at FROM configuration_versions WHERE project_id=$1 AND environment_id=$2 ORDER BY version DESC LIMIT 1`, projectID, envID).Scan(&version, &raw, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		success(c, http.StatusOK, gin.H{"version": nil, "config": nil, "published_at": nil})
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	success(c, http.StatusOK, gin.H{"version": version, "config": json.RawMessage(raw), "published_at": at})
}

// applyRuntimeConfig switches the active definitions to cfg and returns the progression
// version that new players will pin to. A new progression version is created only
// when the thresholds differ from the latest one.
func applyRuntimeConfig(ctx context.Context, tx pgx.Tx, p, e, configVersionID string, cfg runtimeConfig) (int, bool, error) {
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

// --- Per-definition draft editing (spec 5.2) ---

// draftEdit changes a parsed draft in place and returns the response, or an error to
// send instead (in which case nothing is saved).
type draftEdit func(draft map[string]json.RawMessage) (status int, result any, fail *apiError)

type apiError struct {
	status        int
	code, message string
	details       any
}

// editDraft applies edit to the environment's draft under a row lock.
func (s *Server) editDraft(c *gin.Context, edit draftEdit) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	tx, err := s.db.Begin(c)
	if err != nil {
		internal(c, err)
		return
	}
	defer tx.Rollback(c)
	var raw []byte
	if err = tx.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2 FOR UPDATE`, projectID, envID).Scan(&raw); err != nil {
		internal(c, err)
		return
	}
	draft, ok := normalizeDraft(raw)
	if !ok {
		draft, _ = normalizeDraft([]byte("{}"))
	}
	status, result, fail := edit(draft)
	if fail != nil {
		failureDetails(c, fail.status, fail.code, fail.message, fail.details)
		return
	}
	b, err := json.Marshal(draft)
	if err != nil {
		internal(c, err)
		return
	}
	if _, err = tx.Exec(c, `UPDATE configuration_drafts SET config=$3,updated_at=now() WHERE project_id=$1 AND environment_id=$2`, projectID, envID, b); err != nil {
		internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		internal(c, err)
		return
	}
	success(c, status, result)
}

// definitionKind describes one draft collection: how to decode and validate an entry.
type definitionKind struct {
	collection, singular string
	// decode strictly parses one definition and returns it normalized, with its key.
	decode func(raw []byte) (any, string, error)
	// validate reports problems for one definition at path.
	validate func(path string, def any, errs *fieldErrors)
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

var definitionKinds = map[string]definitionKind{
	"items": {
		collection: "items",
		singular:   "item",
		decode: func(raw []byte) (any, string, error) {
			x, err := strictDecode[itemConfig](raw)
			return x, x.Key, err
		},
		validate: func(path string, def any, errs *fieldErrors) { validateItem(path, def.(itemConfig), errs) },
	},
	"currencies": {
		collection: "currencies",
		singular:   "currency",
		decode: func(raw []byte) (any, string, error) {
			x, err := strictDecode[currencyConfig](raw)
			return x, x.Key, err
		},
		validate: func(path string, def any, errs *fieldErrors) { validateCurrency(path, def.(currencyConfig), errs) },
	},
	"leaderboards": {
		collection: "leaderboards",
		singular:   "leaderboard",
		decode: func(raw []byte) (any, string, error) {
			x, err := strictDecode[leaderboardConfig](raw)
			return x, x.Key, err
		},
		validate: func(path string, def any, errs *fieldErrors) {
			validateLeaderboard(path, def.(leaderboardConfig), errs)
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

var errDefinitionNotFound = &apiError{status: http.StatusNotFound, code: "NOT_FOUND", message: "The definition was not found in the draft."}

// readDefinition decodes and validates a request body for kind; pathKey, when set, is
// the key from the URL, which the body must match or omit.
func readDefinition(c *gin.Context, kind definitionKind, pathKey string) (json.RawMessage, string, *apiError) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, "", &apiError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "The request body or fields are invalid."}
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
		return nil, "", &apiError{status: http.StatusBadRequest, code: "INVALID_REQUEST", message: "The definition is not valid JSON for this schema: " + err.Error() + "."}
	}
	if pathKey != "" && key != pathKey {
		return nil, "", &apiError{status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR", message: "The body key must match the URL key.", details: gin.H{"errors": []FieldError{{Field: "key", Message: "must match the URL key"}}}}
	}
	var errs fieldErrors
	kind.validate(kind.singular, def, &errs)
	if len(errs) > 0 {
		return nil, "", &apiError{status: http.StatusUnprocessableEntity, code: "VALIDATION_ERROR", message: "The definition is invalid.", details: gin.H{"errors": errs}}
	}
	normalized, _ := json.Marshal(def)
	return normalized, key, nil
}

func (s *Server) listDefinitions(kind definitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, envID, ok := s.ownedEnvironment(c)
		if !ok {
			return
		}
		var raw []byte
		if err := s.db.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
			internal(c, err)
			return
		}
		draft, _ := normalizeDraft(raw)
		entries, _ := collectionEntries(draft, kind.collection)
		if entries == nil {
			entries = []json.RawMessage{}
		}
		success(c, http.StatusOK, entries)
	}
}

func (s *Server) getDefinition(kind definitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, envID, ok := s.ownedEnvironment(c)
		if !ok {
			return
		}
		var raw []byte
		if err := s.db.QueryRow(c, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
			internal(c, err)
			return
		}
		draft, _ := normalizeDraft(raw)
		entries, keys := collectionEntries(draft, kind.collection)
		if i := slices.Index(keys, c.Param("key")); i >= 0 {
			success(c, http.StatusOK, entries[i])
			return
		}
		failure(c, errDefinitionNotFound.status, errDefinitionNotFound.code, errDefinitionNotFound.message)
	}
}

func (s *Server) createDefinition(kind definitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		def, key, fail := readDefinition(c, kind, "")
		if fail != nil {
			failureDetails(c, fail.status, fail.code, fail.message, fail.details)
			return
		}
		s.editDraft(c, func(draft map[string]json.RawMessage) (int, any, *apiError) {
			entries, keys := collectionEntries(draft, kind.collection)
			if slices.Contains(keys, key) {
				return 0, nil, &apiError{status: http.StatusConflict, code: "CONFLICT", message: fmt.Sprintf("A definition with key %q already exists in the draft.", key)}
			}
			setCollection(draft, kind.collection, append(entries, def))
			return http.StatusCreated, def, nil
		})
	}
}

func (s *Server) replaceDefinition(kind definitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		def, key, fail := readDefinition(c, kind, c.Param("key"))
		if fail != nil {
			failureDetails(c, fail.status, fail.code, fail.message, fail.details)
			return
		}
		s.editDraft(c, func(draft map[string]json.RawMessage) (int, any, *apiError) {
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

func (s *Server) deleteDefinition(kind definitionKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Param("key")
		s.editDraft(c, func(draft map[string]json.RawMessage) (int, any, *apiError) {
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

func (s *Server) getDraftProgression(c *gin.Context) {
	projectID, envID, ok := s.ownedEnvironment(c)
	if !ok {
		return
	}
	var raw []byte
	if err := s.db.QueryRow(c, `SELECT config->'progression' FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&raw); err != nil {
		internal(c, err)
		return
	}
	var thresholds []int64
	_ = json.Unmarshal(raw, &thresholds)
	if thresholds == nil {
		thresholds = []int64{}
	}
	success(c, http.StatusOK, gin.H{"thresholds": thresholds})
}

func (s *Server) putDraftProgression(c *gin.Context) {
	var in struct {
		Thresholds []int64 `json:"thresholds"`
	}
	if !bind(c, &in) {
		return
	}
	var errs fieldErrors
	validateProgression("thresholds", in.Thresholds, &errs)
	if len(errs) > 0 {
		failureDetails(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "The progression thresholds are invalid.", gin.H{"errors": errs})
		return
	}
	s.editDraft(c, func(draft map[string]json.RawMessage) (int, any, *apiError) {
		draft["progression"], _ = json.Marshal(in.Thresholds)
		return http.StatusOK, gin.H{"thresholds": in.Thresholds}, nil
	})
}
