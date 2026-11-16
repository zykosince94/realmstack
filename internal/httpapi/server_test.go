package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"realmstack-backend/internal/platform"
)

func TestMalformedIDParamsAre404(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// No database: the ID check must answer before any query runs.
	s := New(nil, Config{JWTSecret: strings.Repeat("s", 40)}, logger)
	token := s.Env.Token(platform.Principal{ID: platform.RandomUUID(), Kind: platform.KindUser}, time.Minute)
	for _, path := range []string{"/v1/credentials/not-a-uuid/revoke", "/v1/server/players/abc/xp/award"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("%s status=%d, want 404", path, rec.Code)
		}
	}
}

func TestErrorEnvelopeHasDetails(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := New(nil, Config{JWTSecret: strings.Repeat("s", 40)}, logger)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, httptest.NewRequest("GET", "/missing", nil))
	var body struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Details == nil {
		t.Fatalf("details missing: %s", rec.Body.String())
	}
}

func TestHealthResponseUsesStandardEnvelopeAndRequestID(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := New(nil, Config{JWTSecret: strings.Repeat("s", 40), BodyLimit: 1024}, logger)
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("request id header missing")
	}
	var body map[string]any
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["data"] == nil || body["meta"] == nil {
		t.Fatalf("response envelope missing: %s", rec.Body.String())
	}
}

func TestUnknownRouteAndPanicUseStandardErrors(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := New(nil, Config{JWTSecret: strings.Repeat("s", 40), BodyLimit: 1024}, logger)
	s.router.GET("/panic-test", func(c *gin.Context) { panic("sensitive panic detail") })
	for _, test := range []struct {
		path   string
		status int
	}{
		{path: "/not-found", status: 404},
		{path: "/panic-test", status: 500},
	} {
		req := httptest.NewRequest("GET", test.path, nil)
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Fatalf("%s status=%d, want %d", test.path, rec.Code, test.status)
		}
		var body map[string]any
		if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&body); err != nil {
			t.Fatalf("%s did not return JSON: %v", test.path, err)
		}
		if body["error"] == nil || body["meta"] == nil {
			t.Fatalf("%s response envelope missing: %s", test.path, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "sensitive panic detail") {
			t.Fatal("panic details leaked in response")
		}
	}
}

func TestRateLimiterWindow(t *testing.T) {
	l := newRateLimiter(2, time.Minute)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("k", now); !ok {
			t.Fatalf("attempt %d rejected", i+1)
		}
	}
	ok, retry := l.allow("k", now)
	if ok || retry <= 0 {
		t.Fatalf("third attempt allowed=%v retry=%v", ok, retry)
	}
	if ok, _ = l.allow("other", now); !ok {
		t.Fatal("limit leaked across keys")
	}
	if ok, _ = l.allow("k", now.Add(time.Minute)); !ok {
		t.Fatal("window did not reset")
	}
}

func loginAttempts(s *Server, forwardedFor []string) []int {
	statuses := make([]int, 0, len(forwardedFor))
	for _, ip := range forwardedFor {
		req := httptest.NewRequest("POST", "/v1/auth/login", strings.NewReader(`{"email":"a@b.c","password":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", ip)
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		statuses = append(statuses, rec.Code)
	}
	return statuses
}

func TestRateLimitIgnoresSpoofedForwardedFor(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// No database: requests that pass the limiter fail later, which is fine here.
	s := New(nil, Config{JWTSecret: strings.Repeat("s", 40), AuthRateLimit: 2}, logger)
	statuses := loginAttempts(s, []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"})
	if statuses[2] != 429 {
		t.Fatalf("spoofed X-Forwarded-For bypassed the limiter: %v", statuses)
	}
}

func TestRateLimitUsesForwardedForFromTrustedProxy(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// httptest requests come from 192.0.2.1, which is trusted here.
	s := New(nil, Config{JWTSecret: strings.Repeat("s", 40), AuthRateLimit: 2, TrustedProxies: []string{"192.0.2.0/24"}}, logger)
	statuses := loginAttempts(s, []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"})
	for _, status := range statuses {
		if status == 429 {
			t.Fatalf("distinct clients behind a trusted proxy were rate limited together: %v", statuses)
		}
	}
	if statuses = loginAttempts(s, []string{"203.0.113.9", "203.0.113.9", "203.0.113.9"}); statuses[2] != 429 {
		t.Fatalf("same client behind a trusted proxy was not limited: %v", statuses)
	}
}

func TestHTTPServerHasTimeouts(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := New(nil, Config{JWTSecret: strings.Repeat("s", 40)}, logger).HTTPServer(":0")
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Fatalf("server timeouts not set: %+v", srv)
	}
}
