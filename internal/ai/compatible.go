package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// Provider IDs stored in ai_provider_settings.provider.
const (
	ProviderOpenAI     = "openai"
	ProviderCompatible = "openai_compatible"
)

// CompatibleProvider talks to any service that implements OpenAI's Chat Completions API
// (OpenRouter, Together, Groq, vLLM, Ollama, ...), at a base URL the user chooses.
//
// It sends the same strict JSON Schema as the OpenAI adapter, as a Chat Completions
// response_format. Whether a given model enforces it is the model's business: the
// output is decoded strictly and checked by catalog.CheckConfig either way, so a model
// that ignores the schema ends in an "invalid" or "failed" generation, never in a bad
// draft. It deliberately omits OpenAI-only parameters (reasoning effort, store) that
// other services reject, and uses max_tokens, the name every compatible service accepts.
type CompatibleProvider struct {
	client openai.Client
	model  string
	label  string // the service's host, for messages
}

// NewCompatible builds the provider with the SSRF-guarded HTTP client.
func NewCompatible(s Settings, allowPrivate bool) (Provider, error) {
	return newCompatible(s, allowPrivate)
}

func newCompatible(s Settings, allowPrivate bool, opts ...option.RequestOption) (*CompatibleProvider, error) {
	if s.Provider != ProviderCompatible {
		return nil, fmt.Errorf("unsupported provider %q", s.Provider)
	}
	base, err := ValidateBaseURL(s.BaseURL, allowPrivate)
	if err != nil {
		return nil, err
	}
	opts = append([]option.RequestOption{
		option.WithAPIKey(s.APIKey), option.WithBaseURL(base), option.WithMaxRetries(2),
		option.WithHTTPClient(guardedClient(allowPrivate)),
	}, opts...)
	return &CompatibleProvider{client: openai.NewClient(opts...), model: s.Model, label: hostLabel(base)}, nil
}

func (p *CompatibleProvider) Generate(ctx context.Context, req Request) (Result, error) {
	resp, err := p.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: shared.ChatModel(p.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(req.Instructions),
			openai.UserMessage(req.Input),
		},
		MaxTokens: openai.Int(req.MaxTokens),
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name: req.SchemaName, Schema: req.Schema, Strict: openai.Bool(true),
				},
			},
		},
	})
	if err != nil {
		return Result{}, p.translate(ctx, err)
	}
	out := Result{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
	// Some services answer 200 with an error body and no choices.
	if len(resp.Choices) == 0 {
		return out, &ProviderError{Code: CodeMalformedOutput, Message: "The provider returned no answer. Check the model and base URL."}
	}
	choice := resp.Choices[0]
	switch {
	case choice.Message.Refusal != "" || choice.FinishReason == "content_filter":
		return out, &ProviderError{Code: CodeRefused, Message: "The model declined the request."}
	case choice.FinishReason == "length":
		return out, &ProviderError{Code: CodeOutputTruncated, Message: "The output hit the token limit before it was complete. Ask for fewer definitions."}
	}
	out.JSON = stripJSONFence(choice.Message.Content)
	if out.JSON == "" {
		return out, &ProviderError{Code: CodeMalformedOutput, Message: "The provider returned no output."}
	}
	return out, nil
}

// Check is what saving the settings runs, and what "re-check" runs later. It costs no
// tokens. The key is verified at GET /key where the service has one (OpenRouter does);
// a service without it answers 404 and the model list decides. The model must appear in
// GET /models; a list that cannot be read is skipped, because the first real request will
// say. Many services have no model-retrieval route, which is why this does not use one.
func (p *CompatibleProvider) Check(ctx context.Context) error {
	var key json.RawMessage
	if err := p.client.Get(ctx, "key", nil, &key); err != nil {
		if pe := (*ProviderError)(nil); errors.As(p.translate(ctx, err), &pe) && pe.Code == CodeKeyRejected {
			return pe
		}
		// No such endpoint, or it failed for another reason: the model list decides.
	}
	var raw json.RawMessage
	if err := p.client.Get(ctx, "models", nil, &raw); err != nil {
		return p.translate(ctx, err)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &list) != nil || len(list.Data) == 0 {
		return nil // a list this code cannot read: the first real request will tell
	}
	for _, m := range list.Data {
		if m.ID == p.model {
			return nil
		}
	}
	return &ProviderError{Code: CodeModelNotFound, Message: p.label + " does not list the model \"" + p.model + "\". Check the model name."}
}

// translate maps an SDK error to a ProviderError with hints for a user-chosen endpoint.
// Like Query Pulse's adapter it never repeats the service's own error text: a service can
// echo part of the key back, and the status code says what to do.
func (p *CompatibleProvider) translate(ctx context.Context, err error) error {
	err = translate(ctx, err)
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case CodeKeyRejected:
		pe.Message = p.label + " rejected the API key. Check that the API key field holds your key, not the model ID, and that the key belongs to " + p.label + "."
	case CodeModelNotFound:
		pe.Message = p.label + " does not know that model for this key, or the base URL is wrong (it usually ends in /v1). Check the model name."
	case CodeRequestRejected:
		if strings.Contains(pe.Message, "HTTP 400") || strings.Contains(pe.Message, "HTTP 422") {
			pe.Message += " The model may not support strict JSON Schema output; try another model."
		}
	}
	return pe
}

// hostLabel names the service in messages: its host, or a generic word.
func hostLabel(base string) string {
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "The provider"
}

// stripJSONFence removes a Markdown code fence some models wrap around their JSON.
func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}

// ValidateBaseURL checks a user-supplied endpoint and returns it normalised (no trailing
// slash). Users, not operators, choose this URL, so the server would otherwise call
// whatever address a dashboard user types (server-side request forgery): https is
// required, credentials, queries and fragments are rejected, and literal private
// addresses are refused. allowPrivate (AI_ALLOW_PRIVATE_URLS, for local models such as
// Ollama) lifts the https and private-address rules. Hostnames are checked again at
// connect time (guardedClient), which also defeats DNS tricks.
func ValidateBaseURL(raw string, allowPrivate bool) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "":
		return "", errors.New("must be a full URL such as https://openrouter.ai/api/v1")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return "", errors.New("must not contain credentials, a query or a fragment")
	case len(raw) > 300:
		return "", errors.New("is too long")
	case u.Scheme != "https" && !(allowPrivate && u.Scheme == "http"):
		return "", errors.New("must use https")
	}
	if !allowPrivate {
		if addr, perr := netip.ParseAddr(u.Hostname()); perr == nil && !publicAddr(addr) {
			return "", errors.New("must not point at a private or local address")
		}
		if h := strings.ToLower(u.Hostname()); h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".internal") {
			return "", errors.New("must not point at a private or local address")
		}
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// publicAddr reports whether addr is a globally routable unicast address.
func publicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	// Carrier-grade NAT (100.64.0.0/10) is not private in the stdlib's sense but is
	// not the public internet either.
	return !netip.MustParsePrefix("100.64.0.0/10").Contains(addr)
}

var errBlockedAddress = errors.New("the provider address is private or local, which is not allowed")

// guardedClient returns an HTTP client that refuses to connect to non-public addresses
// (unless allowPrivate) and never follows redirects, so a public host cannot bounce the
// request, and the key, to an internal one. The check runs on the address actually
// being dialled, after DNS resolution. It ignores proxy environment variables, which
// would otherwise move the connection (and the check) to the proxy.
func guardedClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errBlockedAddress
			}
			addr, err := netip.ParseAddr(host)
			if err != nil || !publicAddr(addr) {
				return errBlockedAddress
			}
			return nil
		},
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
