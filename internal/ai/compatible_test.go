package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/option"
)

// A local server stands in for an OpenAI-compatible service, so the adapter's real
// request and response handling is tested without a network call or a key. The server
// is on loopback, so these tests allow private addresses; the guard has its own tests.
func fakeCompatible(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body map[string]any)) *CompatibleProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		handle(w, r, body)
	}))
	t.Cleanup(srv.Close)
	p, err := newCompatible(Settings{Provider: ProviderCompatible, Model: "nvidia/nemotron-3.5-super-vl:free", APIKey: "or-test-key-0000000000", BaseURL: srv.URL + "/api/v1"}, true, option.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func completion(content, refusal, finish string) string {
	c, _ := json.Marshal(content)
	r := `null`
	if refusal != "" {
		b, _ := json.Marshal(refusal)
		r = string(b)
	}
	return `{"id":"c1","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"finish_reason":"` + finish + `",
		"message":{"role":"assistant","content":` + string(c) + `,"refusal":` + r + `}}],
		"usage":{"prompt_tokens":120,"completion_tokens":45,"total_tokens":165}}`
}

func TestCompatibleRequestShapeAndParsing(t *testing.T) {
	var seen map[string]any
	var auth, path string
	p := fakeCompatible(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		seen, auth, path = body, r.Header.Get("Authorization"), r.URL.Path
		_, _ = io.WriteString(w, completion("```json\n{\"answer\":\"ok\"}\n```", "", "stop"))
	})
	res, err := p.Generate(context.Background(), Request{Instructions: "rules", Input: "config", SchemaName: "realmstack_explain", Schema: schemaFor(OpExplain), Effort: "medium", MaxTokens: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if res.JSON != `{"answer":"ok"}` || res.InputTokens != 120 || res.OutputTokens != 45 {
		t.Fatalf("result: %+v", res)
	}
	if path != "/api/v1/chat/completions" || auth != "Bearer or-test-key-0000000000" {
		t.Fatalf("path %q auth %q", path, auth)
	}
	format := seen["response_format"].(map[string]any)
	schema := format["json_schema"].(map[string]any)
	messages := seen["messages"].([]any)
	if seen["model"] != "nvidia/nemotron-3.5-super-vl:free" || seen["max_tokens"].(float64) != 5000 ||
		format["type"] != "json_schema" || schema["strict"] != true || schema["name"] != "realmstack_explain" ||
		len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[0].(map[string]any)["content"] != "rules" ||
		messages[1].(map[string]any)["role"] != "user" || messages[1].(map[string]any)["content"] != "config" {
		t.Fatalf("request body: %v", seen)
	}
	// OpenAI-only parameters would be rejected by other services.
	for _, k := range []string{"store", "reasoning", "reasoning_effort", "max_completion_tokens"} {
		if _, ok := seen[k]; ok {
			t.Errorf("request carries %q", k)
		}
	}
}

func TestCompatibleErrorsAndHints(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		code     string
		contains string
	}{
		{"truncated", 200, completion(`{"answer":"cut`, "", "length"), CodeOutputTruncated, ""},
		{"filtered", 200, completion("", "", "content_filter"), CodeRefused, ""},
		{"refusal", 200, completion("", "I can't help with that.", "stop"), CodeRefused, ""},
		{"empty", 200, completion("", "", "stop"), CodeMalformedOutput, ""},
		{"error in a 200", 200, `{"error":{"message":"No endpoints found","code":404}}`, CodeMalformedOutput, "base URL"},
		{"bad key points at the key field", 401, `{"error":{"message":"Missing Authentication header","code":401}}`, CodeKeyRejected, "not the model ID"},
		{"a service echoing the key is not repeated", 401, `{"error":{"message":"Invalid key or-test-key-0000000000 for user"}}`, CodeKeyRejected, "127.0.0.1"},
		{"no credit", 402, `{"error":{"message":"Insufficient credits"}}`, CodeRequestRejected, "credit"},
		{"unknown model or path", 404, `{"error":{"message":"not found"}}`, CodeModelNotFound, "base URL"},
		{"no structured output", 400, `{"error":{"message":"response_format not supported"}}`, CodeRequestRejected, "JSON Schema"},
		{"rate limit", 429, `{"error":{"message":"slow down"}}`, CodeRateLimited, ""},
		{"down", 503, `{"error":{"message":"overloaded"}}`, CodeProviderDown, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := fakeCompatible(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			_, err := p.Generate(context.Background(), Request{Schema: schemaFor(OpExplain), SchemaName: "x", MaxTokens: 10})
			var pe *ProviderError
			if !errors.As(err, &pe) || pe.Code != c.code {
				t.Fatalf("err=%v, want %s", err, c.code)
			}
			if c.contains != "" && !strings.Contains(pe.Message, c.contains) {
				t.Fatalf("message %q lacks %q", pe.Message, c.contains)
			}
			if strings.Contains(pe.Message, "or-test") {
				t.Fatal("error message contains the key")
			}
		})
	}
}

// Check costs no tokens: GET /key verifies the key where the service has one, GET
// /models verifies the model. Many compatible services have no model-retrieval route.
func TestCompatibleCheck(t *testing.T) {
	const model = "nvidia/nemotron-3.5-super-vl:free"
	models := `{"data":[{"id":"openrouter/auto"},{"id":"` + model + `"}]}`
	cases := []struct {
		name  string
		serve func(w http.ResponseWriter, path string)
		code  string // "" = success
	}{
		{"key and model fine", func(w http.ResponseWriter, path string) {
			if strings.HasSuffix(path, "/key") {
				_, _ = io.WriteString(w, `{"data":{"label":"k"}}`)
				return
			}
			_, _ = io.WriteString(w, models)
		}, ""},
		{"no /key route: the model list decides", func(w http.ResponseWriter, path string) {
			if strings.HasSuffix(path, "/key") {
				w.WriteHeader(404)
				return
			}
			_, _ = io.WriteString(w, models)
		}, ""},
		{"key rejected", func(w http.ResponseWriter, path string) {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"Missing Authentication header"}}`)
		}, CodeKeyRejected},
		{"no /key route but the list rejects the key", func(w http.ResponseWriter, path string) {
			if strings.HasSuffix(path, "/key") {
				w.WriteHeader(404)
				return
			}
			w.WriteHeader(401)
		}, CodeKeyRejected},
		{"model not listed", func(w http.ResponseWriter, path string) {
			if strings.HasSuffix(path, "/key") {
				_, _ = io.WriteString(w, `{}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"other/model"}]}`)
		}, CodeModelNotFound},
		{"an unreadable list is skipped", func(w http.ResponseWriter, path string) {
			_, _ = io.WriteString(w, `[{"id":"x"}]`)
		}, ""},
		{"an empty list is skipped", func(w http.ResponseWriter, path string) {
			_, _ = io.WriteString(w, `{"data":[]}`)
		}, ""},
		{"service down", func(w http.ResponseWriter, path string) { w.WriteHeader(503) }, CodeProviderDown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var seen []string
			var auth string
			p := fakeCompatible(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
				seen = append(seen, r.Method+" "+r.URL.Path)
				auth = r.Header.Get("Authorization")
				c.serve(w, r.URL.Path)
			})
			err := p.Check(context.Background())
			if c.code == "" {
				if err != nil {
					t.Fatalf("err: %v", err)
				}
			} else {
				var pe *ProviderError
				if !errors.As(err, &pe) || pe.Code != c.code {
					t.Fatalf("err=%v, want %s", err, c.code)
				}
			}
			for _, call := range seen {
				if strings.Contains(call, "chat/completions") {
					t.Fatalf("check generated text: %s", call)
				}
			}
			if auth != "Bearer or-test-key-0000000000" {
				t.Fatalf("auth header %q", auth)
			}
		})
	}
}

func TestStripJSONFence(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:                 `{"a":1}`,
		"  {\"a\":1}\n":           `{"a":1}`,
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"```\n{\"a\":1}\n```\n":   `{"a":1}`,
		"```json\n{\"a\":1}":      `{"a":1}`,
		"":                        "",
		"```":                     "",
	} {
		if got := stripJSONFence(in); got != want {
			t.Errorf("stripJSONFence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	good := map[string]string{
		"https://openrouter.ai/api/v1":      "https://openrouter.ai/api/v1",
		"  https://openrouter.ai/api/v1/  ": "https://openrouter.ai/api/v1",
		"https://api.groq.com/openai/v1":    "https://api.groq.com/openai/v1",
		"https://8.8.8.8/v1":                "https://8.8.8.8/v1",
	}
	for in, want := range good {
		if got, err := ValidateBaseURL(in, false); err != nil || got != want {
			t.Errorf("ValidateBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "openrouter.ai/api/v1", "not a url", "ftp://openrouter.ai/v1", "http://openrouter.ai/v1",
		"https://user:pass@openrouter.ai/v1", "https://openrouter.ai/v1?key=1", "https://openrouter.ai/v1#x",
		"https://localhost/v1", "https://app.localhost/v1", "https://metadata.internal/v1",
		"https://127.0.0.1/v1", "https://10.0.0.5/v1", "https://192.168.1.1/v1", "https://172.16.0.1/v1",
		"https://169.254.169.254/latest", "https://[::1]/v1", "https://[fe80::1]/v1", "https://100.64.0.1/v1",
		"https://0.0.0.0/v1", "https://[::ffff:127.0.0.1]/v1", "https://" + strings.Repeat("a", 300) + ".com/v1",
	}
	for _, in := range bad {
		if got, err := ValidateBaseURL(in, false); err == nil {
			t.Errorf("ValidateBaseURL(%q) accepted as %q", in, got)
		}
	}
	// A local model server needs http and a private address, which the operator opts into.
	for in, want := range map[string]string{"http://localhost:11434/v1": "http://localhost:11434/v1", "http://host.docker.internal:11434/v1/": "http://host.docker.internal:11434/v1", "https://10.0.0.5/v1": "https://10.0.0.5/v1"} {
		if got, err := ValidateBaseURL(in, true); err != nil || got != want {
			t.Errorf("allowPrivate ValidateBaseURL(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ValidateBaseURL("https://user:pass@localhost/v1", true); err == nil {
		t.Error("credentials must be rejected even when private addresses are allowed")
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.5.5": false, "192.168.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "100.127.255.254": false, "0.0.0.0": false, "224.0.0.1": false, "::1": false, "fe80::1": false,
		"fc00::1": false, "::ffff:10.0.0.1": false, "::ffff:8.8.8.8": true,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The guard runs on the address actually dialled, so a hostname that resolves to a
// private address (or DNS rebinding) is blocked the same as a literal one, and a
// redirect cannot bounce a request to an internal host.
func TestGuardedClientBlocksPrivateAddressesAndRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "reached")
	}))
	defer srv.Close()

	if resp, err := guardedClient(false).Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("connected to a loopback address with private addresses disallowed")
	} else if !errors.Is(err, errBlockedAddress) && !strings.Contains(err.Error(), errBlockedAddress.Error()) {
		t.Fatalf("unexpected error: %v", err)
	}

	client := guardedClient(true)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "reached" {
		t.Fatalf("body %q", body)
	}
	resp, err = client.Get(srv.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect was followed (status %d)", resp.StatusCode)
	}
}

func TestDefaultFactoryPicksTheAdapter(t *testing.T) {
	s := &Service{}
	if p, err := s.defaultFactory(Settings{Provider: ProviderOpenAI, Model: "gpt-5.5", APIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	} else if _, ok := p.(*OpenAIProvider); !ok {
		t.Fatalf("openai gave %T", p)
	}
	compat := Settings{Provider: ProviderCompatible, Model: "vendor/model", APIKey: "k", BaseURL: "https://openrouter.ai/api/v1"}
	if p, err := s.defaultFactory(compat); err != nil {
		t.Fatal(err)
	} else if _, ok := p.(*CompatibleProvider); !ok {
		t.Fatalf("compatible gave %T", p)
	}
	// Settings written before an operator tightened the rules are rechecked at use.
	compat.BaseURL = "https://127.0.0.1/v1"
	if _, err := s.defaultFactory(compat); err == nil {
		t.Fatal("a private base URL was accepted")
	}
	s.cfg.AllowPrivateURLs = true
	if _, err := s.defaultFactory(compat); err != nil {
		t.Fatalf("allowed private URL: %v", err)
	}
	if _, err := s.defaultFactory(Settings{Provider: "acme"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
