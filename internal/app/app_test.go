package app

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
)

func TestValidateConfig(t *testing.T) {
	valid := demoRuntimeConfiguration()
	if err := validateConfig(valid); err != nil {
		t.Fatalf("demo config rejected: %v", err)
	}
	invalid := valid
	invalid.Progression = []int64{100, 100}
	if err := validateConfig(invalid); err == nil {
		t.Fatal("expected non-increasing XP thresholds to be rejected")
	}
	invalid = valid
	invalid.Items = append(invalid.Items, valid.Items[0])
	if err := validateConfig(invalid); err == nil {
		t.Fatal("expected duplicate item keys to be rejected")
	}
}

func TestJWTAccessTokenKindAndExpiry(t *testing.T) {
	s := &Server{cfg: Config{JWTSecret: strings.Repeat("x", 40)}}
	p := principal{ID: "player-1", ProjectID: "project-1", EnvironmentID: "environment-1", Kind: "player"}
	token := s.token(p, time.Minute)
	parsed, ok := s.readJWT("Bearer "+token, "player")
	if !ok || parsed.ID != p.ID || parsed.EnvironmentID != p.EnvironmentID {
		t.Fatalf("valid player token rejected: %#v %v", parsed, ok)
	}
	if _, ok = s.readJWT("Bearer "+token, "user"); ok {
		t.Fatal("player token accepted as user token")
	}
	s.cfg.AccessTTL = -time.Second
	if _, ok = s.readJWT("Bearer "+s.token(p, s.cfg.AccessTTL), "player"); ok {
		t.Fatal("expired token accepted")
	}
}

func TestCredentialHashIsOneWayAndConstantTimeComparable(t *testing.T) {
	secret := "server-secret"
	stored := digest(secret)
	if !secureHash(stored, secret) {
		t.Fatal("valid secret did not match")
	}
	if secureHash(stored, "wrong") {
		t.Fatal("invalid secret matched")
	}
	if stored == secret {
		t.Fatal("credential was not hashed")
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

func TestRandomUUIDShape(t *testing.T) {
	id := randomUUID()
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Fatalf("invalid UUID: %q", id)
	}
}
