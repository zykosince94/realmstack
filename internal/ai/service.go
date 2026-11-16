package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/platform"
)

// Config is the assistant's deployment configuration.
type Config struct {
	// EncryptionKey (AI_ENCRYPTION_KEY, base64, 32 bytes). Empty disables the assistant;
	// every other feature works unchanged (spec 2.8: AI is optional).
	EncryptionKey   string
	Timeout         time.Duration // per generation, including the provider's retries
	MaxOutputTokens int64         // output token cap per call
	PerMinute       int           // generations a user may start per minute
	MaxRunning      int           // generations a user may have in flight
}

// Service holds the assistant's state. Handler methods are mounted by httpapi.
type Service struct {
	*platform.Env
	cfg    Config
	sealer *Sealer
	// NewProvider builds a provider client from decrypted settings; tests replace it.
	NewProvider Factory
	// Sync runs generations inside the request instead of in the background, so tests
	// see the finished result without polling.
	Sync bool

	jobs    sync.WaitGroup
	limitMu sync.Mutex
	starts  map[string][]time.Time
}

// NewService validates cfg. An invalid encryption key is an error (the operator meant
// to enable AI); an empty one disables the assistant.
func NewService(env *platform.Env, cfg Config) (*Service, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Minute
	}
	if cfg.MaxOutputTokens <= 0 {
		cfg.MaxOutputTokens = 32000
	}
	if cfg.PerMinute <= 0 {
		cfg.PerMinute = 6
	}
	if cfg.MaxRunning <= 0 {
		cfg.MaxRunning = 2
	}
	s := &Service{Env: env, cfg: cfg, NewProvider: NewOpenAI, starts: map[string][]time.Time{}}
	if cfg.EncryptionKey != "" {
		sealer, err := NewSealer(cfg.EncryptionKey)
		if err != nil {
			return nil, err
		}
		s.sealer = sealer
	}
	return s, nil
}

// Enabled reports whether the assistant is configured on this deployment.
func (s *Service) Enabled() bool { return s.sealer != nil }

// Wait blocks until background generations finish (graceful shutdown, tests).
func (s *Service) Wait() { s.jobs.Wait() }

// RecoverAbandoned marks generations that were running when the process stopped as
// failed. They run in-process, so after a restart nothing will ever finish them.
func RecoverAbandoned(ctx context.Context, q platform.Querier) error {
	_, err := q.Exec(ctx, `UPDATE ai_generations SET status='failed',error_code=$1,error_message='The server restarted while this generation was running.',finished_at=now()
		WHERE status IN ('pending','running')`, CodeAbandonedByRestart)
	return err
}

var errUnavailable = &platform.Error{Status: http.StatusServiceUnavailable, Code: "AI_UNAVAILABLE", Message: "The AI assistant is not enabled on this deployment (AI_ENCRYPTION_KEY is not set). Every other feature works without it."}

// RequireEnabled answers 503 on AI routes when the assistant is off.
func (s *Service) RequireEnabled() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.Enabled() {
			platform.Respond(c, errUnavailable)
			c.Abort()
			return
		}
		c.Next()
	}
}

// --- Settings ---

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$`)

// Providers lists the supported providers.
var Providers = []string{"openai"}

func (s *Service) settingsView(c *gin.Context, userID string) (gin.H, error) {
	var provider, model, last4 string
	var updated time.Time
	err := s.DB.QueryRow(c, `SELECT provider,model,key_last4,updated_at FROM ai_provider_settings WHERE user_id=$1`, userID).Scan(&provider, &model, &last4, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return gin.H{"configured": false, "provider": nil, "model": nil, "key_last4": nil, "updated_at": nil, "providers": Providers}, nil
	}
	if err != nil {
		return nil, err
	}
	return gin.H{"configured": true, "provider": provider, "model": model, "key_last4": last4, "updated_at": updated, "providers": Providers}, nil
}

// GetSettings shows the signed-in user's provider settings. The key itself is never
// returned, only its last four characters.
func (s *Service) GetSettings(c *gin.Context) {
	view, err := s.settingsView(c, platform.GetPrincipal(c).ID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, view)
}

// PutSettings stores provider settings. api_key may be omitted to change only the
// model of an existing setting.
func (s *Service) PutSettings(c *gin.Context) {
	var in struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		APIKey   string `json:"api_key"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	userID := platform.GetPrincipal(c).ID
	in.Model, in.APIKey = strings.TrimSpace(in.Model), strings.TrimSpace(in.APIKey)
	var errs platform.FieldErrors
	if in.Provider != "openai" {
		errs.Add("provider", "must be one of %s", strings.Join(Providers, ", "))
	}
	if !modelPattern.MatchString(in.Model) {
		errs.Add("model", "must be a model ID such as gpt-5.5")
	}
	if in.APIKey != "" && (len(in.APIKey) < 20 || len(in.APIKey) > 400 || strings.ContainsAny(in.APIKey, " \t\r\n")) {
		errs.Add("api_key", "does not look like an API key")
	}
	if len(errs) > 0 {
		platform.Respond(c, platform.Invalid("VALIDATION_ERROR", "The AI settings are invalid.", errs))
		return
	}
	if in.APIKey == "" {
		tag, err := s.DB.Exec(c, `UPDATE ai_provider_settings SET provider=$2,model=$3,updated_at=now() WHERE user_id=$1`, userID, in.Provider, in.Model)
		if err != nil {
			platform.Internal(c, err)
			return
		}
		if tag.RowsAffected() == 0 {
			platform.Respond(c, platform.Invalid("VALIDATION_ERROR", "The AI settings are invalid.", []platform.FieldError{{Field: "api_key", Message: "is required the first time"}}))
			return
		}
	} else {
		// The user ID is bound into the ciphertext, so a key copied onto another user's
		// row does not decrypt.
		sealed, nonce, err := s.sealer.Seal(in.APIKey, []byte(userID))
		if err != nil {
			platform.Internal(c, err)
			return
		}
		_, err = s.DB.Exec(c, `INSERT INTO ai_provider_settings(user_id,provider,model,encrypted_api_key,key_nonce,key_version,key_last4) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(user_id) DO UPDATE SET provider=EXCLUDED.provider,model=EXCLUDED.model,encrypted_api_key=EXCLUDED.encrypted_api_key,
			key_nonce=EXCLUDED.key_nonce,key_version=EXCLUDED.key_version,key_last4=EXCLUDED.key_last4,updated_at=now()`,
			userID, in.Provider, in.Model, sealed, nonce, s.sealer.Version, in.APIKey[len(in.APIKey)-4:])
		if err != nil {
			platform.Internal(c, err)
			return
		}
	}
	view, err := s.settingsView(c, userID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, view)
}

// DeleteSettings removes the stored key. Past generations stay for the record.
func (s *Service) DeleteSettings(c *gin.Context) {
	if _, err := s.DB.Exec(c, `DELETE FROM ai_provider_settings WHERE user_id=$1`, platform.GetPrincipal(c).ID); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, gin.H{"deleted": true})
}

var errNotConfigured = &platform.Error{Status: http.StatusConflict, Code: "AI_NOT_CONFIGURED", Message: "Add a provider API key in AI settings first."}

// loadSettings decrypts the user's settings. The plaintext key lives only in memory
// for the duration of the call.
func (s *Service) loadSettings(ctx context.Context, userID string) (Settings, error) {
	var st Settings
	var sealed, nonce []byte
	err := s.DB.QueryRow(ctx, `SELECT provider,model,encrypted_api_key,key_nonce FROM ai_provider_settings WHERE user_id=$1`, userID).Scan(&st.Provider, &st.Model, &sealed, &nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, errNotConfigured
	}
	if err != nil {
		return st, err
	}
	if st.APIKey, err = s.sealer.Open(sealed, nonce, []byte(userID)); err != nil {
		return st, &platform.Error{Status: http.StatusConflict, Code: "AI_KEY_UNREADABLE", Message: "The stored key can no longer be decrypted (was AI_ENCRYPTION_KEY changed?). Enter it again."}
	}
	return st, nil
}

// TestSettings checks the stored key and model against the provider.
func (s *Service) TestSettings(c *gin.Context) {
	st, err := s.loadSettings(c, platform.GetPrincipal(c).ID)
	if err != nil {
		platform.Respond(c, err)
		return
	}
	provider, err := s.NewProvider(st)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	ctx, cancel := context.WithTimeout(c, 20*time.Second)
	defer cancel()
	if err = provider.Check(ctx); err != nil {
		respondProvider(c, err)
		return
	}
	platform.Success(c, http.StatusOK, gin.H{"ok": true, "provider": st.Provider, "model": st.Model})
}

func respondProvider(c *gin.Context, err error) {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		platform.Internal(c, err)
		return
	}
	status := http.StatusBadGateway
	switch pe.Code {
	case CodeKeyRejected, CodeModelNotFound:
		status = http.StatusUnprocessableEntity
	case CodeRateLimited:
		status = http.StatusTooManyRequests
	}
	platform.Respond(c, &platform.Error{Status: status, Code: pe.Code, Message: pe.Message})
}

// --- Generations ---

const generationColumns = `id::text,operation,params,merge_policy,provider,model,status,output,errors,notes,error_code,error_message,
	input_tokens,output_tokens,created_at,finished_at,applied_at`

func scanGeneration(row pgx.Row) (gin.H, error) {
	var id, op, policy, provider, model, status, notes, code, message string
	var params, output, errs []byte
	var in, out int64
	var created time.Time
	var finished, applied *time.Time
	if err := row.Scan(&id, &op, &params, &policy, &provider, &model, &status, &output, &errs, &notes, &code, &message, &in, &out, &created, &finished, &applied); err != nil {
		return nil, err
	}
	var outcome struct {
		Proposal *json.RawMessage `json:"proposal"`
		Answer   *json.RawMessage `json:"answer"`
	}
	if len(output) > 0 {
		_ = json.Unmarshal(output, &outcome)
	}
	var failure any
	if code != "" {
		failure = gin.H{"code": code, "message": message}
	}
	return gin.H{
		"id": id, "operation": op, "params": json.RawMessage(params), "merge_policy": policy, "provider": provider, "model": model,
		"status": status, "proposal": outcome.Proposal, "answer": outcome.Answer, "notes": notes, "errors": json.RawMessage(errs),
		"error": failure, "usage": gin.H{"input_tokens": in, "output_tokens": out},
		"created_at": created, "finished_at": finished, "applied_at": applied, "appliable": status == "valid",
	}, nil
}

// allowStart enforces the per-user start rate (in process, like the auth limiter;
// Phase 8 moves limits to a shared store).
func (s *Service) allowStart(userID string, now time.Time) bool {
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	recent := s.starts[userID][:0]
	for _, t := range s.starts[userID] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= s.cfg.PerMinute {
		s.starts[userID] = recent
		return false
	}
	s.starts[userID] = append(recent, now)
	return true
}

// CreateGeneration starts a generation. The provider call can take minutes, longer
// than the HTTP write timeout, so it runs in the background: the response is 202 with
// the pending generation, and the dashboard polls GET .../generations/{id}.
func (s *Service) CreateGeneration(c *gin.Context) {
	var in struct {
		Operation   string `json:"operation"`
		Params      Params `json:"params"`
		MergePolicy string `json:"merge_policy"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	if in.MergePolicy == "" {
		in.MergePolicy = "add"
	}
	errs := platform.FieldErrors(in.Params.normalize(in.Operation))
	if in.MergePolicy != "add" && in.MergePolicy != "replace" {
		errs.Add("merge_policy", "must be add or replace")
	}
	if len(errs) > 0 {
		platform.Respond(c, platform.Invalid("VALIDATION_ERROR", "The request is invalid.", errs))
		return
	}
	projectID, envID := platform.Owned(c)
	userID := platform.GetPrincipal(c).ID
	settings, err := s.loadSettings(c, userID)
	if err != nil {
		platform.Respond(c, err)
		return
	}
	var running int
	if err = s.DB.QueryRow(c, `SELECT count(*) FROM ai_generations WHERE user_id=$1 AND status IN ('pending','running')`, userID).Scan(&running); err != nil {
		platform.Internal(c, err)
		return
	}
	if running >= s.cfg.MaxRunning {
		platform.Fail(c, http.StatusTooManyRequests, "AI_BUSY", "Wait for your running generations to finish.")
		return
	}
	if !s.allowStart(userID, s.Now()) {
		c.Header("Retry-After", "60")
		platform.Fail(c, http.StatusTooManyRequests, "RATE_LIMITED", "Too many AI requests; try again in a minute.")
		return
	}
	draft, err := catalog.ReadDraft(c, s.DB, projectID, envID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	params, _ := json.Marshal(in.Params)
	sum := sha256.Sum256(draft)
	var id string
	err = s.DB.QueryRow(c, `INSERT INTO ai_generations(project_id,environment_id,user_id,operation,params,merge_policy,provider,model,status,base_draft_hash)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9) RETURNING id::text`,
		projectID, envID, userID, in.Operation, params, in.MergePolicy, settings.Provider, settings.Model, hex.EncodeToString(sum[:])).Scan(&id)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	job := job{id: id, projectID: projectID, envID: envID, op: in.Operation, params: in.Params, replace: in.MergePolicy == "replace", settings: settings, draft: draft}
	if s.Sync {
		s.run(job)
	} else {
		s.jobs.Add(1)
		go func() {
			defer s.jobs.Done()
			s.run(job)
		}()
	}
	view, err := scanGeneration(s.DB.QueryRow(c, `SELECT `+generationColumns+` FROM ai_generations WHERE id=$1`, id))
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusAccepted, view)
}

type job struct {
	id, projectID, envID, op string
	params                   Params
	replace                  bool
	settings                 Settings
	draft                    []byte
}

// run executes one generation and records its outcome. It uses its own context: the
// request that started it has already returned.
func (s *Service) run(j job) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout+30*time.Second)
	defer cancel()
	started := time.Now()
	fail := func(code, message string, in, out int64) {
		_, _ = s.DB.Exec(ctx, `UPDATE ai_generations SET status='failed',error_code=$2,error_message=$3,input_tokens=$4,output_tokens=$5,finished_at=now() WHERE id=$1`,
			j.id, code, message, in, out)
		s.Log.Info("ai generation failed", "generation", j.id, "operation", j.op, "code", code, "duration_ms", time.Since(started).Milliseconds())
	}
	if _, err := s.DB.Exec(ctx, `UPDATE ai_generations SET status='running' WHERE id=$1`, j.id); err != nil {
		s.Log.Error("ai generation could not start", "generation", j.id, "error", err)
		return
	}
	cfg, _ := catalog.DecodeConfig(j.draft)
	draftContext, err := buildContext(cfg)
	if err != nil {
		fail("AI_CONTEXT_TOO_LARGE", err.Error(), 0, 0)
		return
	}
	provider, err := s.NewProvider(j.settings)
	if err != nil {
		fail(CodeRequestRejected, err.Error(), 0, 0)
		return
	}
	callCtx, callCancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer callCancel()
	result, err := provider.Generate(callCtx, Request{
		Instructions: instructionsFor(j.op, j.params), Input: inputFor(j.op, j.params, draftContext),
		SchemaName: "realmstack_" + j.op, Schema: schemaFor(j.op), Effort: effortFor(j.op), MaxTokens: s.cfg.MaxOutputTokens,
	})
	if err != nil {
		var pe *ProviderError
		if errors.As(err, &pe) {
			fail(pe.Code, pe.Message, result.InputTokens, result.OutputTokens)
		} else {
			fail(CodeProviderDown, "The provider call failed.", result.InputTokens, result.OutputTokens)
		}
		return
	}
	outcome, err := decode(j.op, result.JSON)
	if err != nil {
		fail(CodeMalformedOutput, "The provider's output did not match the expected shape: "+err.Error(), result.InputTokens, result.OutputTokens)
		return
	}
	status, merged, fieldErrs := "answered", []byte(nil), []platform.FieldError{}
	if Generates(j.op) {
		merged, fieldErrs, err = s.check(ctx, j.projectID, j.envID, j.draft, outcome.Proposal, j.replace)
		if err != nil {
			fail(CodeProviderDown, "Could not validate the proposal.", result.InputTokens, result.OutputTokens)
			s.Log.Error("ai proposal validation failed", "generation", j.id, "error", err)
			return
		}
		status = "valid"
		if len(fieldErrs) > 0 {
			status = "invalid"
		}
	}
	output, _ := json.Marshal(outcome)
	errsJSON, _ := json.Marshal(fieldErrs)
	if _, err = s.DB.Exec(ctx, `UPDATE ai_generations SET status=$2,output=$3,merged=$4,errors=$5,notes=$6,input_tokens=$7,output_tokens=$8,finished_at=now() WHERE id=$1`,
		j.id, status, output, merged, errsJSON, outcome.Notes, result.InputTokens, result.OutputTokens); err != nil {
		s.Log.Error("ai generation could not be saved", "generation", j.id, "error", err)
		return
	}
	// Token usage is logged for cost visibility; prompts, outputs and keys never are.
	s.Log.Info("ai generation finished", "generation", j.id, "operation", j.op, "status", status, "errors", len(fieldErrs),
		"input_tokens", result.InputTokens, "output_tokens", result.OutputTokens, "duration_ms", time.Since(started).Milliseconds())
}

// check merges a proposal into draft and validates the result exactly as publish
// would. An invalid proposal is stored with its field errors and can never be applied:
// this is the "AI-generated invalid configuration is rejected" exit criterion.
func (s *Service) check(ctx context.Context, projectID, envID string, draft []byte, p catalog.Proposal, replace bool) ([]byte, []platform.FieldError, error) {
	if p.Empty() {
		return nil, []platform.FieldError{{Field: "proposal", Message: "contains no definitions"}}, nil
	}
	merged, conflicts, err := catalog.MergeProposal(draft, p, replace)
	if err != nil {
		return nil, nil, err
	}
	_, errs, err := catalog.CheckConfig(ctx, s.DB, projectID, envID, merged)
	if err != nil {
		return nil, nil, err
	}
	return merged, append(conflicts, errs...), nil
}

// ListGenerations returns the environment's recent generations, newest first.
func (s *Service) ListGenerations(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	rows, err := s.DB.Query(c, `SELECT `+generationColumns+` FROM ai_generations WHERE project_id=$1 AND environment_id=$2 ORDER BY created_at DESC LIMIT 50`, projectID, envID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		g, err := scanGeneration(rows)
		if err != nil {
			platform.Internal(c, err)
			return
		}
		out = append(out, g)
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}

var errGenerationNotFound = &platform.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "No such generation in this environment."}

// GetGeneration returns one generation (the dashboard polls this while it runs).
func (s *Service) GetGeneration(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	g, err := scanGeneration(s.DB.QueryRow(c, `SELECT `+generationColumns+` FROM ai_generations WHERE id=$1 AND project_id=$2 AND environment_id=$3`, c.Param("id"), projectID, envID))
	if errors.Is(err, pgx.ErrNoRows) {
		platform.Respond(c, errGenerationNotFound)
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, g)
}

// Apply is the human approval: it merges a valid proposal into the draft. It never
// publishes. The proposal is merged into the draft as it is now and validated again,
// because the draft may have changed since the proposal was generated.
func (s *Service) Apply(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	tx, err := s.DB.Begin(c)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer tx.Rollback(c)
	// Lock the draft first (the same order as every draft edit), then the generation.
	draft, err := catalog.LockDraft(c, tx, projectID, envID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	var status, policy, baseHash string
	var output []byte
	err = tx.QueryRow(c, `SELECT status,merge_policy,base_draft_hash,COALESCE(output,'{}'::jsonb) FROM ai_generations WHERE id=$1 AND project_id=$2 AND environment_id=$3 FOR UPDATE`,
		c.Param("id"), projectID, envID).Scan(&status, &policy, &baseHash, &output)
	if errors.Is(err, pgx.ErrNoRows) {
		platform.Respond(c, errGenerationNotFound)
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	switch status {
	case "valid":
	case "applied":
		platform.Fail(c, http.StatusConflict, "ALREADY_APPLIED", "This proposal was already applied to the draft.")
		return
	default:
		platform.Fail(c, http.StatusUnprocessableEntity, "NOT_APPLICABLE", "Only a valid proposal can be applied; this one is "+status+".")
		return
	}
	var outcome Outcome
	if err = json.Unmarshal(output, &outcome); err != nil {
		platform.Internal(c, err)
		return
	}
	merged, fieldErrs, err := s.check(c, projectID, envID, draft, outcome.Proposal, policy == "replace")
	if err != nil {
		platform.Internal(c, err)
		return
	}
	if len(fieldErrs) > 0 {
		sum := sha256.Sum256(draft)
		if hex.EncodeToString(sum[:]) != baseHash {
			platform.FailDetails(c, http.StatusConflict, "DRAFT_CHANGED", "The draft changed since this proposal was generated, and the proposal no longer fits it.", gin.H{"errors": fieldErrs})
			return
		}
		platform.Respond(c, platform.Invalid("INVALID_CONFIGURATION", "The proposal no longer validates.", fieldErrs))
		return
	}
	if err = catalog.WriteDraft(c, tx, projectID, envID, merged); err != nil {
		platform.Internal(c, err)
		return
	}
	if _, err = tx.Exec(c, `UPDATE ai_generations SET status='applied',applied_at=$2,applied_by=$3 WHERE id=$1`, c.Param("id"), s.Now(), platform.GetPrincipal(c).ID); err != nil {
		platform.Internal(c, err)
		return
	}
	if err = tx.Commit(c); err != nil {
		platform.Internal(c, err)
		return
	}
	g, err := scanGeneration(s.DB.QueryRow(c, `SELECT `+generationColumns+` FROM ai_generations WHERE id=$1`, c.Param("id")))
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, g)
}

// Discard closes a finished generation without applying it.
func (s *Service) Discard(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	tag, err := s.DB.Exec(c, `UPDATE ai_generations SET status='discarded' WHERE id=$1 AND project_id=$2 AND environment_id=$3 AND status IN ('valid','invalid','answered','failed')`,
		c.Param("id"), projectID, envID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		_ = s.DB.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM ai_generations WHERE id=$1 AND project_id=$2 AND environment_id=$3)`, c.Param("id"), projectID, envID).Scan(&exists)
		if !exists {
			platform.Respond(c, errGenerationNotFound)
			return
		}
		platform.Fail(c, http.StatusConflict, "NOT_DISCARDABLE", "Only a finished, unapplied generation can be discarded.")
		return
	}
	s.GetGeneration(c)
}
