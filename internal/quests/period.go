package quests

import (
	"fmt"
	"time"
)

// Repeat modes (spec 2.7).
const (
	RepeatNone   = "none"
	RepeatDaily  = "daily"
	RepeatWeekly = "weekly"
)

// Period is the repeat period that contains a moment. Acceptances and claims are
// unique per player, quest and period key, which is what lets a daily quest be
// claimed again tomorrow but not twice today.
type Period struct {
	Key        string     // "" for non-repeating, "2026-11-16" daily, "2026-W47" weekly
	Start, End *time.Time // [Start, End); both nil for non-repeating quests
}

// PeriodAt returns the period of the given repeat mode that contains t. Periods are
// UTC: daily resets at 00:00 UTC, weekly at Monday 00:00 UTC. Weekly keys use the ISO
// week-numbering year, so 2026-12-31 (a Thursday) is in 2026-W53 and 2027-01-01 too.
func PeriodAt(repeat string, t time.Time) Period {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	switch repeat {
	case RepeatDaily:
		end := day.AddDate(0, 0, 1)
		return Period{Key: day.Format("2006-01-02"), Start: &day, End: &end}
	case RepeatWeekly:
		// time.Weekday counts from Sunday=0; shift so Monday is the first day.
		start := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
		end := start.AddDate(0, 0, 7)
		year, week := start.ISOWeek()
		return Period{Key: fmt.Sprintf("%04d-W%02d", year, week), Start: &start, End: &end}
	default:
		return Period{}
	}
}
