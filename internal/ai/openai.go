package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// OpenAIProvider calls the OpenAI Responses API with a strict JSON Schema, so the
// model's output is guaranteed to parse into the proposal shape. Validity in the
// game's terms (unknown references, unreachable levels, prerequisite cycles) is still
// checked by catalog.CheckConfig afterwards: the schema only guarantees the shape.
type OpenAIProvider struct {
	client openai.Client
	model  string
}

// NewOpenAI is the production Factory for provider "openai".
func NewOpenAI(s Settings) (Provider, error) { return newOpenAI(s) }

// newOpenAI accepts extra client options; tests point it at a local server.
func newOpenAI(s Settings, opts ...option.RequestOption) (*OpenAIProvider, error) {
	if s.Provider != "openai" {
		return nil, fmt.Errorf("unsupported provider %q", s.Provider)
	}
	// The SDK retries connection errors, 408, 409, 429 and 5xx twice by default; the
	// caller's context deadline bounds the total time including retries.
	opts = append([]option.RequestOption{option.WithAPIKey(s.APIKey), option.WithMaxRetries(2)}, opts...)
	return &OpenAIProvider{client: openai.NewClient(opts...), model: s.Model}, nil
}

func (p *OpenAIProvider) Generate(ctx context.Context, req Request) (Result, error) {
	params := responses.ResponseNewParams{
		Model:           p.model,
		Instructions:    openai.String(req.Instructions),
		Input:           responses.ResponseNewParamsInputUnion{OfString: openai.String(req.Input)},
		MaxOutputTokens: openai.Int(req.MaxTokens),
		// Do not let the provider keep the prompt (draft content) as a stored response.
		Store: openai.Bool(false),
		Text: responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
					Name:   req.SchemaName,
					Schema: req.Schema,
					Strict: openai.Bool(true),
				},
			},
		},
	}
	if req.Effort != "" {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(req.Effort)}
	}
	resp, err := p.client.Responses.New(ctx, params)
	if err != nil {
		return Result{}, translate(ctx, err)
	}
	out := Result{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	switch resp.Status {
	case responses.ResponseStatusIncomplete:
		if resp.IncompleteDetails.Reason == "content_filter" {
			return out, &ProviderError{Code: CodeRefused, Message: "The provider declined to produce this output."}
		}
		return out, &ProviderError{Code: CodeOutputTruncated, Message: "The output hit the token limit before it was complete. Ask for fewer definitions."}
	case responses.ResponseStatusFailed:
		return out, &ProviderError{Code: CodeProviderDown, Message: "The provider reported a failed response: " + resp.Error.Message}
	}
	for _, item := range resp.Output {
		for _, content := range item.Content {
			if content.Type == "refusal" {
				return out, &ProviderError{Code: CodeRefused, Message: "The model declined the request."}
			}
		}
	}
	out.JSON = resp.OutputText()
	if out.JSON == "" {
		return out, &ProviderError{Code: CodeMalformedOutput, Message: "The provider returned no output."}
	}
	return out, nil
}

func (p *OpenAIProvider) Check(ctx context.Context) error {
	if _, err := p.client.Models.Get(ctx, p.model); err != nil {
		return translate(ctx, err)
	}
	return nil
}

// translate maps SDK errors to ProviderErrors. It reads only the status code and the
// Retry-After header; the raw error can contain request headers, so it is never logged
// or returned.
func translate(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &ProviderError{Code: CodeTimeout, Message: "The provider did not answer in time."}
	}
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return &ProviderError{Code: CodeProviderDown, Message: "Could not reach the provider."}
	}
	switch status := apiErr.StatusCode; {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &ProviderError{Code: CodeKeyRejected, Message: "The provider rejected the API key. Check or replace it in AI settings."}
	case status == http.StatusNotFound:
		return &ProviderError{Code: CodeModelNotFound, Message: "The provider does not offer that model to this key."}
	case status == http.StatusTooManyRequests:
		return &ProviderError{Code: CodeRateLimited, Message: "The provider's rate limit or quota for this key was reached.", RetryAfter: retryAfter(apiErr.Response)}
	case status >= 500:
		return &ProviderError{Code: CodeProviderDown, Message: "The provider is unavailable. Try again shortly."}
	default:
		return &ProviderError{Code: CodeRequestRejected, Message: fmt.Sprintf("The provider rejected the request (HTTP %d).", status)}
	}
}

func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}
