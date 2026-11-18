package ai

import (
	"context"
	"fmt"
	"time"
)

// Provider is one AI vendor behind the assistant: OpenAI (Responses API) or any
// OpenAI-compatible service (Chat Completions at a user-chosen base URL). Tests use a
// fake, so no test ever calls a real API.
type Provider interface {
	// Generate returns JSON that matches req.Schema.
	Generate(ctx context.Context, req Request) (Result, error)
	// Check verifies that the key is accepted and the model is reachable.
	Check(ctx context.Context) error
}

// Request is one structured-output call.
type Request struct {
	Instructions string         // the system / developer prompt
	Input        string         // the user content: draft context + the person's brief
	SchemaName   string         // a short identifier for the schema
	Schema       map[string]any // strict JSON Schema the output must match
	Effort       string         // reasoning effort: "low", "medium" or "high"
	MaxTokens    int64          // output token cap
}

// Result is a successful call.
type Result struct {
	JSON         string
	InputTokens  int64
	OutputTokens int64
}

// Settings is a user's provider configuration with the key decrypted (in memory only).
type Settings struct {
	Provider string
	Model    string
	APIKey   string
	BaseURL  string // openai_compatible only
}

// Error codes the assistant reports. They are stable API values.
const (
	CodeKeyRejected        = "AI_KEY_REJECTED"
	CodeModelNotFound      = "AI_MODEL_NOT_FOUND"
	CodeRateLimited        = "AI_RATE_LIMITED"
	CodeProviderDown       = "AI_PROVIDER_UNAVAILABLE"
	CodeRequestRejected    = "AI_REQUEST_REJECTED"
	CodeTimeout            = "AI_TIMEOUT"
	CodeOutputTruncated    = "AI_OUTPUT_TRUNCATED"
	CodeRefused            = "AI_REFUSED"
	CodeMalformedOutput    = "AI_MALFORMED_OUTPUT"
	CodeAbandonedByRestart = "ABANDONED_BY_RESTART"
)

// ProviderError is a failed call, already translated into something a person can act
// on. It never contains the API key or the provider's raw response body.
type ProviderError struct {
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Factory builds a Provider for a user's settings. The server holds one; tests swap it
// for a fake.
type Factory func(Settings) (Provider, error)
