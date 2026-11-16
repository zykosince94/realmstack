package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/option"
)

// A local server stands in for the OpenAI API, so the adapter's real request and
// response handling is tested without a network call or a key.
func fakeOpenAI(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body map[string]any)) *OpenAIProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		handle(w, r, body)
	}))
	t.Cleanup(srv.Close)
	p, err := newOpenAI(Settings{Provider: "openai", Model: "gpt-test", APIKey: "sk-test-key-0000000000"}, option.WithBaseURL(srv.URL+"/v1/"), option.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func response(status, text, incomplete string) string {
	content := `[]`
	if text != "" {
		b, _ := json.Marshal(text)
		content = `[{"type":"output_text","text":` + string(b) + `,"annotations":[]}]`
	}
	details := `null`
	if incomplete != "" {
		details = `{"reason":"` + incomplete + `"}`
	}
	return `{"id":"resp_1","object":"response","status":"` + status + `","incomplete_details":` + details + `,"error":null,
		"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":` + content + `}],
		"usage":{"input_tokens":120,"output_tokens":45,"total_tokens":165}}`
}

func TestOpenAIRequestShapeAndParsing(t *testing.T) {
	var seen map[string]any
	var auth string
	p := fakeOpenAI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("path %s", r.URL.Path)
		}
		seen, auth = body, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, response("completed", `{"answer":"ok"}`, ""))
	})
	res, err := p.Generate(context.Background(), Request{Instructions: "rules", Input: "config", SchemaName: "realmstack_explain", Schema: schemaFor(OpExplain), Effort: "medium", MaxTokens: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if res.JSON != `{"answer":"ok"}` || res.InputTokens != 120 || res.OutputTokens != 45 {
		t.Fatalf("result: %+v", res)
	}
	if auth != "Bearer sk-test-key-0000000000" {
		t.Fatalf("auth header: %q", auth)
	}
	format := seen["text"].(map[string]any)["format"].(map[string]any)
	if seen["store"] != false || seen["model"] != "gpt-test" || seen["max_output_tokens"].(float64) != 5000 || seen["instructions"] != "rules" ||
		format["type"] != "json_schema" || format["strict"] != true || format["name"] != "realmstack_explain" ||
		seen["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Fatalf("request body: %v", seen)
	}
}

func TestOpenAIIncompleteRefusalAndErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"truncated", 200, response("incomplete", `{"answer":"cut`, "max_output_tokens"), CodeOutputTruncated},
		{"filtered", 200, response("incomplete", "", "content_filter"), CodeRefused},
		{"refusal", 200, `{"id":"r","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"I can't help with that."}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, CodeRefused},
		{"empty", 200, response("completed", "", ""), CodeMalformedOutput},
		{"bad key", 401, `{"error":{"message":"Incorrect API key","type":"invalid_request_error","code":"invalid_api_key"}}`, CodeKeyRejected},
		{"quota", 429, `{"error":{"message":"quota","type":"insufficient_quota"}}`, CodeRateLimited},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := fakeOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			_, err := p.Generate(context.Background(), Request{Schema: schemaFor(OpExplain), SchemaName: "x", MaxTokens: 10})
			var pe *ProviderError
			if !errors.As(err, &pe) || pe.Code != c.code {
				t.Fatalf("err=%v, want %s", err, c.code)
			}
			if strings.Contains(pe.Message, "sk-test") {
				t.Fatal("error message contains the key")
			}
		})
	}
}

func TestOpenAICheckRetrievesModel(t *testing.T) {
	var path string
	p := fakeOpenAI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		path = r.URL.Path
		_, _ = io.WriteString(w, `{"id":"gpt-test","object":"model","created":0,"owned_by":"openai"}`)
	})
	if err := p.Check(context.Background()); err != nil || !strings.HasSuffix(path, "/models/gpt-test") {
		t.Fatalf("check: %v %s", err, path)
	}
	missing := fakeOpenAI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"message":"no such model"}}`)
	})
	var pe *ProviderError
	if err := missing.Check(context.Background()); !errors.As(err, &pe) || pe.Code != CodeModelNotFound {
		t.Fatalf("missing model: %v", err)
	}
}
