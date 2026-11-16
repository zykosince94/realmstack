package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// fields returns the field paths of validation errors.
func fields(errs []FieldError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Field
	}
	return out
}

func TestValidateConfigReturnsFieldPaths(t *testing.T) {
	if errs := validateConfig(demoRuntimeConfiguration()); len(errs) != 0 {
		t.Fatalf("demo config rejected: %v", errs)
	}
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		change func(*runtimeConfig)
		want   []string
	}{
		{"non-increasing thresholds", func(c *runtimeConfig) { c.Progression = []int64{100, 100} }, []string{"progression[1]"}},
		{"zero threshold", func(c *runtimeConfig) { c.Progression = []int64{0, 100} }, []string{"progression[0]"}},
		{"empty progression", func(c *runtimeConfig) { c.Progression = nil }, []string{"progression"}},
		{"duplicate item", func(c *runtimeConfig) { c.Items = append(c.Items, c.Items[0]) }, []string{"items[2].key"}},
		{"non-string slot", func(c *runtimeConfig) {
			c.Items = append(c.Items, itemConfig{Key: "bad_slot", Name: "Bad", MaxStack: 1, Metadata: map[string]any{"slot": 5}})
		}, []string{"items[2].metadata.slot"}},
		{"non-stackable stack size", func(c *runtimeConfig) { c.Items[1].MaxStack = 5 }, []string{"items[1].max_stack"}},
		{"bad currency", func(c *runtimeConfig) { c.Currencies[1] = currencyConfig{Key: "Bad Key"} }, []string{"currencies[1].key", "currencies[1].name"}},
		{"duplicate leaderboard", func(c *runtimeConfig) { c.Leaderboards = append(c.Leaderboards, c.Leaderboards[0]) }, []string{"leaderboards[1].key"}},
		{"bad direction and season", func(c *runtimeConfig) {
			c.Leaderboards[0].Direction = "sideways"
			c.Leaderboards[0].SeasonStart, c.Leaderboards[0].SeasonEnd = &start, &start
		}, []string{"leaderboards[0].direction", "leaderboards[0].season_end"}},
		{"several problems at once", func(c *runtimeConfig) {
			c.Progression = []int64{5, 1}
			c.Items[0].Name = ""
		}, []string{"items[0].name", "progression[1]"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := demoRuntimeConfiguration()
			test.change(&cfg)
			if got := fields(validateConfig(cfg)); strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("fields=%v want %v", got, test.want)
			}
		})
	}
}

func TestDecodeConfigReportsShapeErrors(t *testing.T) {
	if _, errs := decodeConfig([]byte(`{"progression":"fast"}`)); len(errs) != 1 || errs[0].Field != "progression" {
		t.Fatalf("errs=%v", errs)
	}
}

func TestLevelFor(t *testing.T) {
	thresholds := []int64{100, 300}
	for xp, want := range map[int64]int{0: 1, 99: 1, 100: 2, 299: 2, 300: 3, 5000: 3} {
		if got := levelFor(thresholds, xp); got != want {
			t.Fatalf("levelFor(%d)=%d want %d", xp, got, want)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 16, 10, 0, 0, 123456000, time.UTC)
	id := randomUUID()
	gotAt, gotID, ok := decodeCursor(encodeCursor(at, id))
	if !ok || !gotAt.Equal(at) || gotID != id {
		t.Fatalf("round trip: %v %v %v", gotAt, gotID, ok)
	}
	for _, bad := range []string{"", "!!", encodeCursor(at, "not-a-uuid")} {
		if _, _, ok := decodeCursor(bad); ok {
			t.Fatalf("accepted bad cursor %q", bad)
		}
	}
}

func TestMalformedIDParamsAre404(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// No database: the ID check must answer before any query runs.
	s := New(nil, Config{JWTSecret: strings.Repeat("s", 40)}, logger)
	token := s.token(principal{ID: randomUUID(), Kind: "user"}, time.Minute)
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

func TestJWTAccessTokenKindAndExpiry(t *testing.T) {
	s := &Server{cfg: Config{JWTSecret: strings.Repeat("x", 40)}, now: time.Now}
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

func TestItemSlot(t *testing.T) {
	if slot, ok := itemSlot(map[string]any{"slot": "weapon"}); !ok || slot != "weapon" {
		t.Fatalf("slot=%q ok=%v", slot, ok)
	}
	if _, ok := itemSlot(map[string]any{"healing": 50}); ok {
		t.Fatal("item without slot reported as equippable")
	}
	if _, ok := itemSlot(nil); ok {
		t.Fatal("nil metadata reported as equippable")
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

func TestDefaultDisplayNameIsNotDerivedFromEmail(t *testing.T) {
	name := defaultDisplayName()
	if !strings.HasPrefix(name, "Player-") || len(name) != len("Player-")+6 {
		t.Fatalf("unexpected display name %q", name)
	}
	if name == defaultDisplayName() {
		t.Fatal("display names should be random")
	}
}

func TestCheckPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(true, string(hash), "correct horse") {
		t.Fatal("correct password rejected")
	}
	if checkPassword(true, string(hash), "wrong") {
		t.Fatal("wrong password accepted")
	}
	if checkPassword(false, "", "anything") {
		t.Fatal("missing account accepted")
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if !isUniqueViolation(fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"})) {
		t.Fatal("wrapped unique violation not detected")
	}
	if isUniqueViolation(&pgconn.PgError{Code: "23503"}) || isUniqueViolation(errors.New("unique")) {
		t.Fatal("non-unique error reported as unique violation")
	}
}

func TestHTTPServerHasTimeouts(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := New(nil, Config{JWTSecret: strings.Repeat("s", 40)}, logger).HTTPServer(":0")
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Fatalf("server timeouts not set: %+v", srv)
	}
}
