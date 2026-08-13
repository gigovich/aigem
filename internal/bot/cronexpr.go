package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed 5-field cron expression (minute hour day-of-month month day-of-week).
type Schedule struct {
	min, hour, dom, month, dow uint64 // bitsets of allowed values
	domStar, dowStar           bool   // field was exactly "*" (standard dom/dow OR semantics)
}

// ParseSchedule parses "minute hour day-of-month month day-of-week".
func ParseSchedule(expr string) (Schedule, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return Schedule{}, fmt.Errorf("cron expression must have 5 fields, got %d: %q", len(f), expr)
	}
	var s Schedule
	var err error
	if s.min, _, err = parseField(f[0], 0, 59); err != nil {
		return Schedule{}, fmt.Errorf("minute: %w", err)
	}
	if s.hour, _, err = parseField(f[1], 0, 23); err != nil {
		return Schedule{}, fmt.Errorf("hour: %w", err)
	}
	if s.dom, s.domStar, err = parseField(f[2], 1, 31); err != nil {
		return Schedule{}, fmt.Errorf("day-of-month: %w", err)
	}
	if s.month, _, err = parseField(f[3], 1, 12); err != nil {
		return Schedule{}, fmt.Errorf("month: %w", err)
	}
	if s.dow, s.dowStar, err = parseField(f[4], 0, 6); err != nil {
		return Schedule{}, fmt.Errorf("day-of-week: %w", err)
	}
	return s, nil
}

// parseField parses one field into a bitset and reports whether it was exactly "*".
func parseField(field string, min, max int) (uint64, bool, error) {
	star := field == "*"
	var bits uint64
	for _, part := range strings.Split(field, ",") {
		step := 1
		rng := part
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n < 1 {
				return 0, false, fmt.Errorf("bad step in %q", part)
			}
			step = n
			rng = part[:i]
		}
		lo, hi := min, max
		if rng != "*" {
			if j := strings.Index(rng, "-"); j >= 0 {
				a, err1 := strconv.Atoi(rng[:j])
				b, err2 := strconv.Atoi(rng[j+1:])
				if err1 != nil || err2 != nil {
					return 0, false, fmt.Errorf("bad range %q", part)
				}
				lo, hi = a, b
			} else {
				v, err := strconv.Atoi(rng)
				if err != nil {
					return 0, false, fmt.Errorf("bad value %q", part)
				}
				lo, hi = v, v
			}
		}
		if lo < min || hi > max || lo > hi {
			return 0, false, fmt.Errorf("value %q out of range %d-%d", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, star, nil
}

// Matches reports whether the schedule fires at minute-resolution time t.
func (s Schedule) Matches(t time.Time) bool {
	return s.matchesDay(t) && s.matchesMinute(t)
}

// matchesDay is the part of the expression that can only change at midnight, so
// Next can reject a whole day without walking its 1440 minutes.
func (s Schedule) matchesDay(t time.Time) bool {
	if s.month&(1<<uint(int(t.Month()))) == 0 {
		return false
	}
	domMatch := s.dom&(1<<uint(t.Day())) != 0
	dowMatch := s.dow&(1<<uint(int(t.Weekday()))) != 0
	// Standard cron: when both day fields are restricted, fire on EITHER; else both must hold
	// (an unrestricted "*" field always matches).
	if !s.domStar && !s.dowStar {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}

func (s Schedule) matchesMinute(t time.Time) bool {
	return s.min&(1<<uint(t.Minute())) != 0 && s.hour&(1<<uint(t.Hour())) != 0
}

// nextHorizonDays bounds how far Next looks ahead. It has to outlast the rarest
// expression that legitimately fires, which is February 29: the gap between two
// of them spans a non-leap century year - 2096 to 2104 - so it is eight years
// and not four. Past that, the bound is what stops an expression that can never
// fire at all ("0 0 30 2 *") from becoming an unbounded loop.
const nextHorizonDays = 366 * 9

// Next returns the first minute strictly after t at which the schedule fires,
// and reports whether it found one inside the horizon.
//
// Days are tested before minutes, so a schedule that fires once a month costs
// one comparison per skipped day rather than 1440. Time is advanced in the
// caller's location: a job written "0 4 * * *" means four in the morning where
// the operator lives, and stepping through UTC would move it twice a year.
func (s Schedule) Next(after time.Time) (time.Time, bool) {
	t := after.Truncate(time.Minute).Add(time.Minute)
	for day := 0; day <= nextHorizonDays; day++ {
		end := dayAfter(t)
		if s.matchesDay(t) {
			for ; t.Before(end); t = t.Add(time.Minute) {
				if s.matchesMinute(t) {
					return t, true
				}
			}
		}
		t = end
	}
	return time.Time{}, false
}

// dayAfter is the first instant past t's calendar day, and is always later than
// t - which is what makes the loop above terminate.
//
// The obvious spelling of it does not have that property. A zone that jumps
// forward across its own midnight has no 00:00 on the day after the jump, and
// time.Date resolves a missing local time BACKWARDS, onto the previous date: in
// Atlantic/Azores, time.Date(2026-03-29 00:00) is 2026-03-28 23:00. Used as an
// end-of-day that is a boundary at or before where the walk already stood, so
// it advanced by nothing for its whole horizon and then reported that the job
// was never scheduled at all - for a heartbeat that was armed and due within
// the hour.
func dayAfter(t time.Time) time.Time {
	y, m, d := t.Date()
	next := time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
	// By the minute, which is the resolution the walk itself works at. Stepping
	// by the hour would clear the gap just as well but overshoot a zone whose
	// offset moves by less than one - a handful of pre-1998 zones changed by 15
	// or 30 minutes - and the minutes it skipped would then be walked under the
	// previous day's date test.
	for !next.After(t) {
		next = next.Add(time.Minute)
	}
	return next
}
