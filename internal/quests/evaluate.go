package quests

import (
	"time"

	"realmstack-backend/internal/goals"
)

// Statuses, in precedence order (spec 6.5): the first that applies wins.
const (
	StatusClaimed   = "claimed"   // completion reward claimed this period
	StatusLocked    = "locked"    // a required quest or achievement is not completed
	StatusUpcoming  = "upcoming"  // before the availability window
	StatusExpired   = "expired"   // window closed and the quest is not complete
	StatusCompleted = "completed" // every stage complete, completion not yet claimed
	StatusActive    = "active"    // accepted (or auto-accepted) and in progress
	StatusAvailable = "available" // can be accepted
)

// CompletionStage is the stage_index of a quest's completion reward claim.
const CompletionStage = -1

// Stage is one ordered step of a published quest.
type Stage struct {
	Key        string            `json:"key"`
	Name       string            `json:"name"`
	Conditions []goals.Condition `json:"conditions"`
	Rewards    goals.Rewards     `json:"rewards"`
}

// Definition is a published, active quest.
type Definition struct {
	ID                   string
	Key                  string
	Name                 string
	Description          string
	Chain                string
	RequiresQuests       []string
	RequiresAchievements []string
	Stages               []Stage
	Rewards              goals.Rewards
	ClaimableBy          string
	AutoAccept           bool
	Repeat               string
	AvailableFrom        *time.Time
	AvailableUntil       *time.Time
	ActivatedAt          time.Time
}

// Acceptance is a player's acceptance record for one quest and period.
type Acceptance struct {
	Active     bool // false once abandoned
	AcceptedAt time.Time
}

// AchievementState is what quests need to know about a required achievement.
type AchievementState struct {
	Completed bool       // conditions met (or already claimed)
	ClaimedAt *time.Time // when it was claimed, if it was
}

// Player is everything about one player that quest evaluation reads, loaded up front
// so evaluation itself is deterministic and side-effect free.
type Player struct {
	// Acceptances by quest key, for each quest's current period only.
	Acceptances map[string]Acceptance
	// PeriodClaims by quest key → stage index → claim time, current period only.
	PeriodClaims map[string]map[int]time.Time
	// FirstCompletion by quest key: the earliest completion claim in any period.
	FirstCompletion map[string]time.Time
	Achievements    map[string]AchievementState
}

// FactsLoader returns the player's ledger facts for a counting window.
type FactsLoader func(w goals.Window) (*goals.Facts, error)

// StageState is the evaluated state of one stage.
type StageState struct {
	Progress []goals.Progress
	Met      bool // conditions met inside the window, regardless of earlier stages
	Complete bool // met, and every earlier stage is complete
	Claimed  bool // its reward was claimed this period
}

// State is one quest's evaluated state for one player at one moment.
type State struct {
	Def    *Definition
	Status string
	Period Period
	// Engaged means the quest is accepted (or auto-accepted) in this period and not
	// locked: only then does progress count and can rewards be claimed.
	Engaged      bool
	AutoAccepted bool
	AcceptedAt   *time.Time // effective acceptance time when engaged
	Window       goals.Window
	Stages       []StageState
	CurrentStage int // first incomplete stage; -1 when complete or not engaged
	Complete     bool
	// CompletionClaimed is true when the completion reward was claimed this period.
	CompletionClaimed bool
	ClaimableStages   []int
	ClaimableComplete bool
	LockedByQuests    []string
	LockedByAchievs   []string
}

// Claimable reports whether a claim would pay anything.
func (s *State) Claimable() bool { return len(s.ClaimableStages) > 0 || s.ClaimableComplete }

// Evaluator computes quest states for one player at one moment (spec 6.4, 6.5). It
// resolves prerequisites recursively and memoizes, so each quest is evaluated once.
type Evaluator struct {
	Now    time.Time
	Defs   map[string]*Definition // active quests by key
	Player Player
	Facts  FactsLoader

	memo     map[string]*State
	visiting map[string]bool
	facts    map[[2]int64]*goals.Facts // cache by window bounds
}

// NewEvaluator prepares an evaluator.
func NewEvaluator(now time.Time, defs map[string]*Definition, p Player, facts FactsLoader) *Evaluator {
	return &Evaluator{Now: now, Defs: defs, Player: p, Facts: facts,
		memo: map[string]*State{}, visiting: map[string]bool{}, facts: map[[2]int64]*goals.Facts{}}
}

// completedEver reports whether a quest counts as completed for requires_quests: its
// completion reward was claimed in any period, or, for a non-repeating quest, it is
// complete now (spec 6.5).
func (e *Evaluator) completedEver(key string) (bool, error) {
	if _, ok := e.Player.FirstCompletion[key]; ok {
		return true, nil
	}
	d, ok := e.Defs[key]
	if !ok || d.Repeat != RepeatNone {
		return false, nil
	}
	s, err := e.State(key)
	if err != nil || s == nil {
		return false, err
	}
	return s.Complete, nil
}

// State evaluates one quest. It returns nil for an unknown (or inactive) quest.
func (e *Evaluator) State(key string) (*State, error) {
	if s, ok := e.memo[key]; ok {
		return s, nil
	}
	d, ok := e.Defs[key]
	if !ok {
		return nil, nil
	}
	if e.visiting[key] {
		// Publish rejects prerequisite cycles; this guard only stops a corrupted
		// configuration from recursing forever. A quest on a cycle is never complete.
		return &State{Def: d, Status: StatusLocked, CurrentStage: -1}, nil
	}
	e.visiting[key] = true
	defer delete(e.visiting, key)

	s := &State{Def: d, Period: PeriodAt(d.Repeat, e.Now), CurrentStage: -1, Stages: make([]StageState, len(d.Stages))}
	claims := e.Player.PeriodClaims[key]
	_, s.CompletionClaimed = claims[CompletionStage]

	// Prerequisites. The latest recorded completion among them is when the quest
	// became available, which anchors an auto_accept quest's counting window.
	anchor := d.ActivatedAt
	later := func(t time.Time) {
		if t.After(anchor) {
			anchor = t
		}
	}
	for _, req := range d.RequiresQuests {
		done, err := e.completedEver(req)
		if err != nil {
			return nil, err
		}
		if !done {
			s.LockedByQuests = append(s.LockedByQuests, req)
		} else if t, ok := e.Player.FirstCompletion[req]; ok {
			later(t)
		}
	}
	for _, req := range d.RequiresAchievements {
		a := e.Player.Achievements[req]
		if !a.Completed {
			s.LockedByAchievs = append(s.LockedByAchievs, req)
		} else if a.ClaimedAt != nil {
			later(*a.ClaimedAt)
		}
	}
	locked := len(s.LockedByQuests) > 0 || len(s.LockedByAchievs) > 0
	upcoming := d.AvailableFrom != nil && e.Now.Before(*d.AvailableFrom)

	// Engagement: an acceptance record for this period wins (an abandoned one means
	// the player opted out, even of an auto_accept quest); otherwise an auto_accept
	// quest is active from the moment it became available.
	if acc, ok := e.Player.Acceptances[key]; ok {
		if acc.Active && !locked {
			s.Engaged, s.AcceptedAt = true, &acc.AcceptedAt
		}
	} else if d.AutoAccept && !locked && !upcoming {
		if s.Period.Start != nil {
			later(*s.Period.Start)
		}
		if d.AvailableFrom != nil {
			later(*d.AvailableFrom)
		}
		at := anchor
		s.Engaged, s.AutoAccepted, s.AcceptedAt = true, true, &at
	}

	if s.Engaged {
		// Count from acceptance (or the later start of the period) until the earlier
		// of the period end and the availability window end (spec 6.4).
		from := *s.AcceptedAt
		if s.Period.Start != nil && s.Period.Start.After(from) {
			from = *s.Period.Start
		}
		until := s.Period.End
		if d.AvailableUntil != nil && (until == nil || d.AvailableUntil.Before(*until)) {
			until = d.AvailableUntil
		}
		s.Window = goals.Window{From: &from, Until: until}
		facts, err := e.load(s.Window)
		if err != nil {
			return nil, err
		}
		// Stages complete strictly in order: stage n is complete only when its
		// conditions are met and stages 1…n−1 are complete.
		previous := true
		for i, st := range d.Stages {
			p := goals.Evaluate(st.Conditions, facts)
			met := goals.AllMet(p)
			_, claimed := claims[i]
			s.Stages[i] = StageState{Progress: p, Met: met, Complete: met && previous, Claimed: claimed}
			previous = s.Stages[i].Complete
			if !previous && s.CurrentStage < 0 {
				s.CurrentStage = i
			}
		}
		s.Complete = len(d.Stages) > 0 && previous
		for i, st := range s.Stages {
			if st.Complete && !st.Claimed && !d.Stages[i].Rewards.Empty() {
				s.ClaimableStages = append(s.ClaimableStages, i)
			}
		}
		s.ClaimableComplete = s.Complete && !s.CompletionClaimed
	} else {
		for i := range d.Stages {
			_, claimed := claims[i]
			s.Stages[i] = StageState{Claimed: claimed}
		}
	}

	closed := d.AvailableUntil != nil && !e.Now.Before(*d.AvailableUntil)
	switch {
	case s.CompletionClaimed:
		s.Status = StatusClaimed
	case locked:
		s.Status = StatusLocked
	case upcoming:
		s.Status = StatusUpcoming
	case closed && !s.Complete:
		s.Status = StatusExpired
	case s.Complete:
		s.Status = StatusCompleted
	case s.Engaged:
		s.Status = StatusActive
	default:
		s.Status = StatusAvailable
	}
	e.memo[key] = s
	return s, nil
}

// load returns facts for a window, loading each distinct window once.
func (e *Evaluator) load(w goals.Window) (*goals.Facts, error) {
	k := [2]int64{-1, -1}
	if w.From != nil {
		k[0] = w.From.UnixNano()
	}
	if w.Until != nil {
		k[1] = w.Until.UnixNano()
	}
	if f, ok := e.facts[k]; ok {
		return f, nil
	}
	f, err := e.Facts(w)
	if err != nil {
		return nil, err
	}
	e.facts[k] = f
	return f, nil
}
