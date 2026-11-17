package leaderboards

import (
	"context"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis orders equal scores by member bytes; that must reproduce PostgreSQL's
// (achieved_at, player_id) tie-break, with UUIDs compared by their bytes.
func TestProjectedMemberOrdersLikePostgres(t *testing.T) {
	base := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	type row struct {
		at     time.Time
		player string
	}
	rows := []row{
		{base.Add(time.Microsecond), "00000000-0000-0000-0000-000000000001"},
		{base, "ffffffff-0000-0000-0000-000000000000"},
		{base, "0fffffff-0000-0000-0000-000000000000"},
		{base.Add(10 * time.Second), "00000000-0000-0000-0000-000000000000"},
		{time.Unix(0, 0), "a0000000-0000-0000-0000-000000000000"},
	}
	byMember := append([]row(nil), rows...)
	sort.Slice(byMember, func(i, j int) bool {
		return projectedMember(byMember[i].at, byMember[i].player) < projectedMember(byMember[j].at, byMember[j].player)
	})
	byTuple := append([]row(nil), rows...)
	sort.Slice(byTuple, func(i, j int) bool {
		if !byTuple[i].at.Equal(byTuple[j].at) {
			return byTuple[i].at.Before(byTuple[j].at)
		}
		return byTuple[i].player < byTuple[j].player
	})
	for i := range rows {
		if byMember[i] != byTuple[i] {
			t.Fatalf("position %d: member order %v, tuple order %v", i, byMember[i], byTuple[i])
		}
	}
}

// The upsert keeps the better entry, so a late or duplicate write of an older best
// cannot overwrite a newer one. Needs TEST_REDIS_URL.
func TestUpsertScriptKeepsTheBetterEntry(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()
	ctx := context.Background()
	board := "test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	sorted, members, _ := projectionKeys(board)
	defer rdb.Del(ctx, sorted, members)
	player := "00000000-0000-0000-0000-00000000000a"
	t0 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	upsert := func(score int64, at time.Time) {
		t.Helper()
		if err := rdb.Eval(ctx, upsertScript, []string{sorted, members}, player, strconv.FormatInt(score, 10), projectedMember(at, player), at.UnixMicro()).Err(); err != nil {
			t.Fatal(err)
		}
	}
	check := func(score int64, at time.Time) {
		t.Helper()
		z, err := rdb.ZRangeWithScores(ctx, sorted, 0, -1).Result()
		if err != nil || len(z) != 1 || z[0].Score != float64(score) || z[0].Member != projectedMember(at, player) {
			t.Fatalf("set = %v (err %v), want one entry %d at %v", z, err, score, at)
		}
		if m, _ := rdb.HGet(ctx, members, player).Result(); m != projectedMember(at, player) {
			t.Fatalf("members hash = %q", m)
		}
	}
	upsert(-100, t0.Add(time.Minute))
	check(-100, t0.Add(time.Minute))
	upsert(-50, t0.Add(2*time.Minute)) // worse score: ignored
	check(-100, t0.Add(time.Minute))
	upsert(-100, t0.Add(3*time.Minute)) // same score, later: ignored
	check(-100, t0.Add(time.Minute))
	upsert(-100, t0) // same score, earlier: replaces
	check(-100, t0)
	upsert(-200, t0.Add(time.Hour)) // better score: replaces
	check(-200, t0.Add(time.Hour))
}
