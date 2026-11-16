package quests

import (
	"testing"
	"time"

	"realmstack-backend/internal/goals"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestPeriodAt(t *testing.T) {
	tests := []struct {
		repeat, at, key, start, end string
	}{
		{RepeatNone, "2026-11-16T12:00:00Z", "", "", ""},
		// Daily: start inclusive, end exclusive, in UTC whatever the input zone.
		{RepeatDaily, "2026-11-16T00:00:00Z", "2026-11-16", "2026-11-16T00:00:00Z", "2026-11-17T00:00:00Z"},
		{RepeatDaily, "2026-11-16T23:59:59.999999Z", "2026-11-16", "2026-11-16T00:00:00Z", "2026-11-17T00:00:00Z"},
		{RepeatDaily, "2026-11-17T00:30:00+02:00", "2026-11-16", "2026-11-16T00:00:00Z", "2026-11-17T00:00:00Z"},
		// Weekly: Monday 00:00 UTC. 2026-11-16 is a Monday.
		{RepeatWeekly, "2026-11-15T23:59:59Z", "2026-W46", "2026-11-09T00:00:00Z", "2026-11-16T00:00:00Z"},
		{RepeatWeekly, "2026-11-16T00:00:00Z", "2026-W47", "2026-11-16T00:00:00Z", "2026-11-23T00:00:00Z"},
		{RepeatWeekly, "2026-11-22T23:59:59Z", "2026-W47", "2026-11-16T00:00:00Z", "2026-11-23T00:00:00Z"},
		// ISO week-numbering year: these days of January 2027 belong to 2026-W53.
		{RepeatWeekly, "2027-01-01T09:00:00Z", "2026-W53", "2026-12-28T00:00:00Z", "2027-01-04T00:00:00Z"},
		{RepeatWeekly, "2027-01-04T00:00:00Z", "2027-W01", "2027-01-04T00:00:00Z", "2027-01-11T00:00:00Z"},
	}
	for _, test := range tests {
		p := PeriodAt(test.repeat, at(test.at))
		if p.Key != test.key {
			t.Errorf("%s %s: key=%q want %q", test.repeat, test.at, p.Key, test.key)
		}
		if test.start == "" {
			if p.Start != nil || p.End != nil {
				t.Errorf("non-repeating period has bounds: %+v", p)
			}
			continue
		}
		if !p.Start.Equal(at(test.start)) || !p.End.Equal(at(test.end)) {
			t.Errorf("%s %s: [%v, %v) want [%s, %s)", test.repeat, test.at, p.Start, p.End, test.start, test.end)
		}
	}
}

// event is one fake ledger row.
type event struct {
	at     time.Time
	stat   string
	amount int64
}

// world is a fake player: ledger events plus flags. Its facts loader applies the
// window exactly like goals.LoadFacts ([From, Until), flags ignore the window).
type world struct {
	events []event
	flags  map[string]bool
	loads  int
}

func (w *world) facts(win goals.Window) (*goals.Facts, error) {
	w.loads++
	f := goals.NewFacts()
	for _, e := range w.events {
		if win.From != nil && e.at.Before(*win.From) {
			continue
		}
		if win.Until != nil && !e.at.Before(*win.Until) {
			continue
		}
		f.Stats[e.stat] += e.amount
	}
	for k, v := range w.flags {
		f.Flags[k] = v
	}
	return f, nil
}

func stage(key string, conds ...goals.Condition) Stage {
	return Stage{Key: key, Name: key, Conditions: conds, Rewards: goals.Rewards{XP: 10}}
}

func kills(n int64) goals.Condition {
	return goals.Condition{Type: goals.Stat, StatKey: "kills", Target: n}
}
func flag(k string) goals.Condition { return goals.Condition{Type: goals.Flag, FlagKey: k} }

var activated = at("2026-01-01T00:00:00Z")

func quest(key string, stages ...Stage) *Definition {
	return &Definition{ID: key + "-id", Key: key, Name: key, Stages: stages, ClaimableBy: "player", Repeat: RepeatNone, ActivatedAt: activated}
}

func newPlayer() Player {
	return Player{Acceptances: map[string]Acceptance{}, PeriodClaims: map[string]map[int]time.Time{}, FirstCompletion: map[string]time.Time{}, Achievements: map[string]AchievementState{}}
}

func state(t *testing.T, now time.Time, defs []*Definition, p Player, w *world, key string) *State {
	t.Helper()
	byKey := map[string]*Definition{}
	for _, d := range defs {
		byKey[d.Key] = d
	}
	s, err := NewEvaluator(now, byKey, p, w.facts).State(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStagesCompleteStrictlyInOrder(t *testing.T) {
	q := quest("q", stage("enter", flag("entered")), stage("fight", kills(3)), stage("return", flag("returned")))
	p := newPlayer()
	accepted := at("2026-03-01T10:00:00Z")
	p.Acceptances["q"] = Acceptance{Active: true, AcceptedAt: accepted}
	// Stage 2's conditions are met, but stage 1's are not: nothing is complete.
	w := &world{events: []event{{accepted.Add(time.Minute), "kills", 3}}, flags: map[string]bool{"returned": true}}
	s := state(t, accepted.Add(time.Hour), []*Definition{q}, p, w, "q")
	if s.Status != StatusActive || s.CurrentStage != 0 || s.Stages[1].Complete || !s.Stages[1].Met || s.Stages[2].Complete {
		t.Fatalf("status=%s current=%d stages=%+v", s.Status, s.CurrentStage, s.Stages)
	}
	if len(s.ClaimableStages) != 0 || s.ClaimableComplete {
		t.Fatalf("nothing should be claimable: %+v", s)
	}
	// Once stage 1 completes, the already-met later stages complete with it.
	w.flags["entered"] = true
	s = state(t, accepted.Add(time.Hour), []*Definition{q}, p, w, "q")
	if s.Status != StatusCompleted || s.CurrentStage != -1 || !s.Complete || len(s.ClaimableStages) != 3 || !s.ClaimableComplete {
		t.Fatalf("status=%s current=%d claimable=%v complete=%v", s.Status, s.CurrentStage, s.ClaimableStages, s.ClaimableComplete)
	}
}

func TestCountingWindowStartsAtAcceptance(t *testing.T) {
	q := quest("q", stage("fight", kills(2)))
	accepted := at("2026-03-01T10:00:00Z")
	w := &world{events: []event{
		{accepted.Add(-time.Microsecond), "kills", 5}, // before acceptance: ignored
		{accepted, "kills", 1},                        // at acceptance: counted (start inclusive)
	}}
	p := newPlayer()
	s := state(t, accepted.Add(time.Hour), []*Definition{q}, p, w, "q")
	if s.Status != StatusAvailable || s.Engaged || s.Stages[0].Progress != nil {
		t.Fatalf("an unaccepted quest has no progress: %+v", s)
	}
	p.Acceptances["q"] = Acceptance{Active: true, AcceptedAt: accepted}
	s = state(t, accepted.Add(time.Hour), []*Definition{q}, p, w, "q")
	if got := s.Stages[0].Progress[0].Current; got != 1 || s.Status != StatusActive {
		t.Fatalf("current=%d status=%s, want 1 active", got, s.Status)
	}
}

func TestAbandonedQuestIsAvailableAndKeepsClaims(t *testing.T) {
	q := quest("q", stage("a", kills(1)), stage("b", kills(5)))
	accepted := at("2026-03-01T10:00:00Z")
	p := newPlayer()
	p.Acceptances["q"] = Acceptance{Active: false, AcceptedAt: accepted}
	p.PeriodClaims["q"] = map[int]time.Time{0: accepted.Add(time.Minute)}
	w := &world{events: []event{{accepted.Add(time.Second), "kills", 9}}}
	s := state(t, accepted.Add(time.Hour), []*Definition{q}, p, w, "q")
	if s.Status != StatusAvailable || s.Engaged || !s.Stages[0].Claimed {
		t.Fatalf("abandoned: %+v", s)
	}
	// Re-accepting later restarts progress; the claimed stage is not claimable again.
	again := accepted.Add(2 * time.Hour)
	p.Acceptances["q"] = Acceptance{Active: true, AcceptedAt: again}
	w.events = append(w.events, event{again.Add(time.Second), "kills", 5})
	s = state(t, again.Add(time.Hour), []*Definition{q}, p, w, "q")
	if !s.Complete || len(s.ClaimableStages) != 1 || s.ClaimableStages[0] != 1 || s.Stages[1].Progress[0].Current != 5 {
		t.Fatalf("re-accepted: complete=%v claimable=%v progress=%+v", s.Complete, s.ClaimableStages, s.Stages[1].Progress)
	}
}

func TestPrerequisitesLockAndUnlockInOrder(t *testing.T) {
	a := quest("a", stage("s", flag("a_done")))
	b := quest("b", stage("s", flag("b_done")))
	b.RequiresQuests = []string{"a"}
	c := quest("c", stage("s", kills(1)))
	c.RequiresQuests = []string{"b"}
	c.RequiresAchievements = []string{"slayer"}
	defs := []*Definition{a, b, c}
	now := at("2026-03-01T10:00:00Z")
	p := newPlayer()
	w := &world{flags: map[string]bool{}}

	if s := state(t, now, defs, p, w, "b"); s.Status != StatusLocked || s.LockedByQuests[0] != "a" {
		t.Fatalf("b should be locked by a: %+v", s)
	}
	// a is non-repeating, so "complete" (not just claimed) unlocks b.
	p.Acceptances["a"] = Acceptance{Active: true, AcceptedAt: now.Add(-time.Hour)}
	w.flags["a_done"] = true
	if s := state(t, now, defs, p, w, "b"); s.Status != StatusAvailable {
		t.Fatalf("b should be available once a is complete: %s", s.Status)
	}
	s := state(t, now, defs, p, w, "c")
	if s.Status != StatusLocked || len(s.LockedByQuests) != 1 || s.LockedByQuests[0] != "b" || s.LockedByAchievs[0] != "slayer" {
		t.Fatalf("c locked by b and slayer: %+v", s)
	}
	p.Acceptances["b"] = Acceptance{Active: true, AcceptedAt: now.Add(-time.Hour)}
	w.flags["b_done"] = true
	p.Achievements["slayer"] = AchievementState{Completed: true}
	if s := state(t, now, defs, p, w, "c"); s.Status != StatusAvailable {
		t.Fatalf("c should be available: %+v", s)
	}
}

func TestRepeatableQuestNeedsAClaimToUnlock(t *testing.T) {
	daily := quest("daily", stage("s", kills(1)))
	daily.Repeat = RepeatDaily
	next := quest("next", stage("s", kills(1)))
	next.RequiresQuests = []string{"daily"}
	now := at("2026-03-01T10:00:00Z")
	p := newPlayer()
	p.Acceptances["daily"] = Acceptance{Active: true, AcceptedAt: now.Add(-time.Hour)}
	w := &world{events: []event{{now.Add(-time.Minute), "kills", 1}}}
	defs := []*Definition{daily, next}
	if s := state(t, now, defs, p, w, "daily"); s.Status != StatusCompleted {
		t.Fatalf("daily: %s", s.Status)
	}
	// Complete but unclaimed does not count for a repeatable quest...
	if s := state(t, now, defs, p, w, "next"); s.Status != StatusLocked {
		t.Fatalf("next: %s", s.Status)
	}
	// ...a completion claim in any period does.
	p.FirstCompletion["daily"] = now.AddDate(0, 0, -3)
	if s := state(t, now, defs, p, w, "next"); s.Status != StatusAvailable {
		t.Fatalf("next: %s", s.Status)
	}
}

func TestDailyQuestResetsAtUTCMidnight(t *testing.T) {
	q := quest("daily", stage("hunt", kills(3)))
	q.Repeat = RepeatDaily
	day1 := at("2026-03-01T22:00:00Z")
	w := &world{events: []event{{day1.Add(time.Minute), "kills", 3}}}
	p := newPlayer()
	p.Acceptances["daily"] = Acceptance{Active: true, AcceptedAt: day1}
	p.PeriodClaims["daily"] = map[int]time.Time{0: day1.Add(time.Hour), CompletionStage: day1.Add(time.Hour)}
	if s := state(t, day1.Add(90*time.Minute), []*Definition{q}, p, w, "daily"); s.Status != StatusClaimed || s.Claimable() {
		t.Fatalf("day 1 after claim: %s claimable=%v", s.Status, s.Claimable())
	}
	// The next UTC day is a new period: the loader would return no acceptance or
	// claims for it, and yesterday's kills fall outside the window.
	day2 := at("2026-03-02T00:00:00Z")
	p2 := newPlayer()
	s := state(t, day2, []*Definition{q}, p2, w, "daily")
	if s.Status != StatusAvailable || s.Period.Key != "2026-03-02" {
		t.Fatalf("day 2: %s %s", s.Status, s.Period.Key)
	}
	// Even an acceptance from before midnight only counts from the period start.
	p2.Acceptances["daily"] = Acceptance{Active: true, AcceptedAt: day1}
	s = state(t, day2.Add(time.Hour), []*Definition{q}, p2, w, "daily")
	if s.Complete || !s.Window.From.Equal(day2) || !s.Window.Until.Equal(day2.AddDate(0, 0, 1)) {
		t.Fatalf("window=%v..%v complete=%v", s.Window.From, s.Window.Until, s.Complete)
	}
}

func TestEventQuestWindow(t *testing.T) {
	from, until := at("2026-06-01T00:00:00Z"), at("2026-06-08T00:00:00Z")
	q := quest("event", stage("s", kills(2)))
	q.AvailableFrom, q.AvailableUntil = &from, &until
	p := newPlayer()
	w := &world{}
	if s := state(t, from.Add(-time.Nanosecond), []*Definition{q}, p, w, "event"); s.Status != StatusUpcoming {
		t.Fatalf("before the window: %s", s.Status)
	}
	if s := state(t, from, []*Definition{q}, p, w, "event"); s.Status != StatusAvailable {
		t.Fatalf("at window start: %s", s.Status)
	}
	p.Acceptances["event"] = Acceptance{Active: true, AcceptedAt: from}
	w.events = []event{
		{until.Add(-time.Microsecond), "kills", 1}, // last instant inside
		{until, "kills", 5},                        // window end is exclusive
	}
	s := state(t, until.Add(time.Hour), []*Definition{q}, p, w, "event")
	if s.Status != StatusExpired || s.Stages[0].Progress[0].Current != 1 {
		t.Fatalf("after the window: %s progress=%+v", s.Status, s.Stages[0].Progress)
	}
	// Completed inside the window: still claimable after it closes.
	w.events = append(w.events, event{from.Add(time.Hour), "kills", 1})
	if s := state(t, until.Add(time.Hour), []*Definition{q}, p, w, "event"); s.Status != StatusCompleted || !s.ClaimableComplete {
		t.Fatalf("completed in window: %s", s.Status)
	}
}

func TestAutoAcceptCountsFromWhenTheQuestBecameAvailable(t *testing.T) {
	first := quest("first", stage("s", flag("x")))
	auto := quest("auto", stage("s", kills(3)))
	auto.AutoAccept = true
	auto.RequiresQuests = []string{"first"}
	claimed := at("2026-03-01T12:00:00Z")
	p := newPlayer()
	p.FirstCompletion["first"] = claimed
	w := &world{events: []event{
		{claimed.Add(-time.Second), "kills", 10}, // before it unlocked: ignored
		{claimed.Add(time.Second), "kills", 2},
	}}
	s := state(t, claimed.Add(time.Hour), []*Definition{first, auto}, p, w, "auto")
	if s.Status != StatusActive || !s.AutoAccepted || !s.AcceptedAt.Equal(claimed) || s.Stages[0].Progress[0].Current != 2 {
		t.Fatalf("auto: %s auto=%v at=%v progress=%+v", s.Status, s.AutoAccepted, s.AcceptedAt, s.Stages[0].Progress)
	}
	// Abandoning records an inactive acceptance: the quest stops counting.
	p.Acceptances["auto"] = Acceptance{Active: false, AcceptedAt: claimed}
	if s := state(t, claimed.Add(time.Hour), []*Definition{first, auto}, p, w, "auto"); s.Status != StatusAvailable || s.Engaged {
		t.Fatalf("abandoned auto quest: %s", s.Status)
	}
	// Without prerequisites, a new auto quest counts from when it was published.
	solo := quest("solo", stage("s", kills(1)))
	solo.AutoAccept = true
	solo.ActivatedAt = claimed
	if s := state(t, claimed.Add(time.Hour), []*Definition{solo}, newPlayer(), w, "solo"); !s.AcceptedAt.Equal(claimed) {
		t.Fatalf("solo anchor: %v", s.AcceptedAt)
	}
}

func TestStatusPrecedence(t *testing.T) {
	// Claimed outranks locked: a prerequisite added later does not hide a claim.
	q := quest("q", stage("s", kills(1)))
	q.RequiresQuests = []string{"missing"}
	p := newPlayer()
	p.PeriodClaims["q"] = map[int]time.Time{CompletionStage: activated}
	if s := state(t, activated.Add(time.Hour), []*Definition{q}, p, &world{}, "q"); s.Status != StatusClaimed {
		t.Fatalf("status=%s", s.Status)
	}
	// Locked outranks upcoming.
	from := activated.Add(48 * time.Hour)
	q2 := quest("q2", stage("s", kills(1)))
	q2.RequiresQuests, q2.AvailableFrom = []string{"missing"}, &from
	if s := state(t, activated, []*Definition{q2}, newPlayer(), &world{}, "q2"); s.Status != StatusLocked {
		t.Fatalf("status=%s", s.Status)
	}
}

func TestPrerequisiteCycleDoesNotRecurseForever(t *testing.T) {
	a := quest("a", stage("s", kills(1)))
	b := quest("b", stage("s", kills(1)))
	a.RequiresQuests, b.RequiresQuests = []string{"b"}, []string{"a"}
	if s := state(t, activated, []*Definition{a, b}, newPlayer(), &world{}, "a"); s.Status != StatusLocked {
		t.Fatalf("status=%s", s.Status)
	}
}

func TestFactsAreLoadedOncePerWindow(t *testing.T) {
	accepted := at("2026-03-01T10:00:00Z")
	a := quest("a", stage("s", kills(1)))
	b := quest("b", stage("s", kills(2)))
	p := newPlayer()
	p.Acceptances["a"] = Acceptance{Active: true, AcceptedAt: accepted}
	p.Acceptances["b"] = Acceptance{Active: true, AcceptedAt: accepted}
	w := &world{}
	e := NewEvaluator(accepted.Add(time.Hour), map[string]*Definition{"a": a, "b": b}, p, w.facts)
	for _, k := range []string{"a", "b", "a"} {
		if _, err := e.State(k); err != nil {
			t.Fatal(err)
		}
	}
	if w.loads != 1 {
		t.Fatalf("loads=%d, want 1 (same window)", w.loads)
	}
}
