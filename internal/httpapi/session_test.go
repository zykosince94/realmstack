package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"realmstack-backend/internal/auth"
)

// refreshCookie returns the session cookie a response set ("" when it cleared it).
func refreshCookie(t *testing.T, r apiResult) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: r.Header}).Cookies() {
		if c.Name == auth.RefreshCookie {
			return c
		}
	}
	return nil
}

func withCookie(value string) map[string]string {
	return map[string]string{"Cookie": auth.RefreshCookie + "=" + value}
}

// Dashboard sessions survive a reload: login sets an httpOnly refresh cookie, refresh
// rotates it and returns a fresh access token, a reused (copied) cookie ends the whole
// sign-in, and logout revokes it.
func TestIntegrationDashboardSessions(t *testing.T) {
	f := newFixture(t)
	login := expect(t, call(t, f.h, "POST", "/v1/auth/register", map[string]any{"email": "session@example.test", "password": "DashboardPass123!"}, nil), 201)
	first := refreshCookie(t, login)
	if first == nil || first.Value == "" {
		t.Fatalf("register set no session cookie: %v", login.Header)
	}
	// Page scripts must never read it, other sites must not send it, and only the auth
	// routes receive it.
	if !first.HttpOnly || first.SameSite != http.SameSiteStrictMode || first.Path != "/v1/auth" || !first.Secure || first.MaxAge <= 0 {
		t.Fatalf("cookie attributes: %+v", first)
	}
	if strings.Contains(login.raw, first.Value) {
		t.Fatal("the refresh token appears in the response body")
	}
	if got := login.obj(t)["expires_in"]; got == nil || got.(float64) <= 0 {
		t.Fatalf("expires_in = %v", got)
	}
	// Only a digest is stored.
	if n := f.count(`SELECT count(*) FROM dashboard_refresh_tokens WHERE token_hash=$1`, first.Value); n != 0 {
		t.Fatal("the refresh token is stored in plaintext")
	}

	// No cookie: nothing to resume.
	if r := expect(t, call(t, f.h, "POST", "/v1/auth/refresh", nil, nil), 401); r.Error.Code != "NO_SESSION" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	if r := expect(t, call(t, f.h, "POST", "/v1/auth/refresh", nil, withCookie("not-a-token")), 401); r.Error.Code != "INVALID_SESSION" || refreshCookie(t, r) == nil || refreshCookie(t, r).MaxAge >= 0 {
		t.Fatalf("an unknown cookie must be rejected and cleared: %s %v", r.Error.Code, r.Header)
	}

	// Refresh: a new access token that works, and a rotated cookie.
	refreshed := expect(t, call(t, f.h, "POST", "/v1/auth/refresh", nil, withCookie(first.Value)), 200)
	second := refreshCookie(t, refreshed)
	if second == nil || second.Value == "" || second.Value == first.Value {
		t.Fatalf("refresh did not rotate the cookie: %+v", second)
	}
	session := refreshed.obj(t)
	if session["user"].(map[string]any)["email"] != "session@example.test" {
		t.Fatalf("session: %v", session)
	}
	expect(t, call(t, f.h, "GET", "/v1/projects", nil, bearer(session["access_token"].(string))), 200)

	// The rotated cookie came back: someone copied it. The whole sign-in ends, including
	// the cookie the legitimate browser holds now.
	if r := expect(t, call(t, f.h, "POST", "/v1/auth/refresh", nil, withCookie(first.Value)), 401); r.Error.Code != "SESSION_REUSE" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	if r := expect(t, call(t, f.h, "POST", "/v1/auth/refresh", nil, withCookie(second.Value)), 401); r.Error.Code != "SESSION_REUSE" {
		t.Fatalf("after reuse the family must be revoked, got %s", r.Error.Code)
	}

	// A fresh sign-in, then logout: the cookie is cleared and no longer works.
	again := expect(t, call(t, f.h, "POST", "/v1/auth/login", map[string]any{"email": "session@example.test", "password": "DashboardPass123!"}, nil), 200)
	third := refreshCookie(t, again)
	out := expect(t, call(t, f.h, "POST", "/v1/auth/logout", nil, withCookie(third.Value)), 200)
	if c := refreshCookie(t, out); c == nil || c.MaxAge >= 0 {
		t.Fatalf("logout did not clear the cookie: %v", out.Header)
	}
	expect(t, call(t, f.h, "POST", "/v1/auth/refresh", nil, withCookie(third.Value)), 401)
	// Logout without a cookie still succeeds, so signing out never fails.
	expect(t, call(t, f.h, "POST", "/v1/auth/logout", nil, nil), 200)
}

// An expired refresh token is rejected even though it was never revoked.
func TestIntegrationDashboardSessionExpiry(t *testing.T) {
	f := newFixture(t)
	srv := newServer(t, f.db)
	f.h = srv.Handler()
	login := expect(t, call(t, f.h, "POST", "/v1/auth/register", map[string]any{"email": "expiry@example.test", "password": "DashboardPass123!"}, nil), 201)
	cookie := refreshCookie(t, login)
	later := time.Now().Add(srv.Env.RefreshTTL + time.Minute)
	srv.Env.Now = func() time.Time { return later }
	if r := expect(t, call(t, f.h, "POST", "/v1/auth/refresh", nil, withCookie(cookie.Value)), 401); r.Error.Code != "INVALID_SESSION" {
		t.Fatalf("code=%s", r.Error.Code)
	}
}
