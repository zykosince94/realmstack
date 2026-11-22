package httpapi

import (
	"context"
	"testing"
	"time"
)

// Activity is derived from when sessions were issued: CCU per 15-minute bucket over 24
// hours, DAU per UTC day over 30 days, scoped to the environment.
func TestIntegrationActivity(t *testing.T) {
	f := newFixture(t) // registering the fixture player issued one session just now
	second, _, _ := f.registerPlayer("second@example.test", "Second")
	// Earlier sessions, stamped directly: the second player was online 3 hours ago and
	// two days ago (twice that day, which still counts once).
	now := time.Now().UTC()
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, at := range []time.Time{ago(3 * time.Hour), ago(48 * time.Hour), ago(48*time.Hour + time.Minute)} {
		if _, err := f.db.Exec(context.Background(), `INSERT INTO refresh_tokens(player_id,family_id,token_hash,expires_at,created_at) VALUES($1,gen_random_uuid(),gen_random_uuid()::text,$2,$3)`,
			second, at.Add(30*24*time.Hour), at); err != nil {
			t.Fatal(err)
		}
	}
	// Another tenant's player must not be counted.
	rival := &fixture{t: t, db: f.db, h: f.h}
	rival.setupTenant("rival")

	var a struct {
		BucketMinutes int   `json:"bucket_minutes"`
		CCUNow        int64 `json:"ccu_now"`
		CCUPeak       int64 `json:"ccu_peak_24h"`
		DAUToday      int64 `json:"dau_today"`
		CCU           []struct {
			At      time.Time `json:"at"`
			Players int64     `json:"players"`
		} `json:"ccu"`
		DAU []struct {
			Date    string `json:"date"`
			Players int64  `json:"players"`
		} `json:"dau"`
	}
	expect(t, call(t, f.h, "GET", f.envPath("/activity"), nil, bearer(f.userToken)), 200).into(t, &a)
	if a.BucketMinutes != 15 || len(a.CCU) != 96 || len(a.DAU) != 30 {
		t.Fatalf("shape: %d buckets of %d min, %d days", len(a.CCU), a.BucketMinutes, len(a.DAU))
	}
	// Both players registered moments ago.
	if a.CCUNow != 2 || a.DAUToday != 2 || a.CCUPeak != 2 {
		t.Fatalf("now=%d today=%d peak=%d, want 2 each", a.CCUNow, a.DAUToday, a.CCUPeak)
	}
	for i := 1; i < len(a.CCU); i++ {
		if got := a.CCU[i].At.Sub(a.CCU[i-1].At); got != 15*time.Minute {
			t.Fatalf("bucket %d is %s after the previous one", i, got)
		}
	}
	bucket := func(at time.Time) int64 {
		for _, p := range a.CCU {
			if !at.Before(p.At) && at.Before(p.At.Add(15*time.Minute)) {
				return p.Players
			}
		}
		t.Fatalf("no bucket holds %s", at)
		return 0
	}
	if got := bucket(ago(3 * time.Hour)); got != 1 {
		t.Fatalf("CCU three hours ago = %d, want 1", got)
	}
	twoDaysAgo := ago(48 * time.Hour).Format("2006-01-02")
	for _, d := range a.DAU {
		if d.Date == twoDaysAgo && d.Players != 1 {
			t.Fatalf("DAU on %s = %d, want 1 (two sessions, one player)", d.Date, d.Players)
		}
	}
	if a.DAU[len(a.DAU)-1].Date != now.Format("2006-01-02") {
		t.Fatalf("the last day is %s, want today", a.DAU[len(a.DAU)-1].Date)
	}

	// Ownership: another dashboard user gets 404, no token 401.
	expect(t, call(t, f.h, "GET", f.envPath("/activity"), nil, bearer(rival.userToken)), 404)
	expect(t, call(t, f.h, "GET", f.envPath("/activity"), nil, nil), 401)
}
