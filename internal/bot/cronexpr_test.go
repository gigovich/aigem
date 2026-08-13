package bot

import (
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return tm
}

func TestParseScheduleMatches(t *testing.T) {
	cases := []struct {
		expr  string
		when  string // "YYYY-MM-DD HH:MM"
		match bool
	}{
		{"* * * * *", "2026-06-20 13:37", true},
		{"0 9 * * *", "2026-06-20 09:00", true},
		{"0 9 * * *", "2026-06-20 09:01", false},
		{"*/15 * * * *", "2026-06-20 10:30", true},
		{"*/15 * * * *", "2026-06-20 10:31", false},
		{"30 2 1 * *", "2026-07-01 02:30", true},
		{"30 2 1 * *", "2026-07-02 02:30", false},
		// 2026-06-20 is a Saturday (weekday 6); 2026-06-22 is Monday (1).
		{"0 9 * * 1-5", "2026-06-22 09:00", true},
		{"0 9 * * 1-5", "2026-06-20 09:00", false},
		// dom/dow OR: fires on the 1st OR any Monday.
		{"0 0 1 * 1", "2026-06-01 00:00", true},  // the 1st (a Monday too)
		{"0 0 1 * 1", "2026-06-08 00:00", true},  // a Monday, not the 1st
		{"0 0 1 * 1", "2026-06-09 00:00", false}, // Tuesday, not the 1st
	}
	for _, c := range cases {
		s, err := ParseSchedule(c.expr)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", c.expr, err)
		}
		if got := s.Matches(at(t, c.when)); got != c.match {
			t.Errorf("%q.Matches(%s) = %v, want %v", c.expr, c.when, got, c.match)
		}
	}
}

func TestParseScheduleErrors(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 0 * *", "* * * 13 *", "* * * * 7", "a * * * *", "*/0 * * * *", "5-1 * * * *",
	} {
		if _, err := ParseSchedule(expr); err == nil {
			t.Errorf("ParseSchedule(%q) should error", expr)
		}
	}
}

func TestScheduleNext(t *testing.T) {
	cases := []struct {
		expr string
		from string // "YYYY-MM-DD HH:MM"
		want string // "" means: no run inside the horizon
	}{
		// Strictly after, so a schedule due at this very minute names the next one
		// rather than the one that is already firing.
		{"* * * * *", "2026-06-20 13:37", "2026-06-20 13:38"},
		{"0 9 * * *", "2026-06-20 09:00", "2026-06-21 09:00"},
		{"0 9 * * *", "2026-06-20 08:59", "2026-06-20 09:00"},
		{"*/15 * * * *", "2026-06-20 10:31", "2026-06-20 10:45"},
		// The minute list a heartbeat installs at tier 0.
		{"7,37 * * * *", "2026-06-20 10:38", "2026-06-20 11:07"},
		// 2026-06-20 is a Saturday; the next weekday 09:00 is Monday the 22nd.
		{"0 9 * * 1-5", "2026-06-20 09:30", "2026-06-22 09:00"},
		// Across a month, and across a year.
		{"30 2 1 * *", "2026-07-01 02:31", "2026-08-01 02:30"},
		{"0 0 1 1 *", "2026-07-01 00:00", "2027-01-01 00:00"},
		// The rarest expression that still fires: February 29, which 2028 has.
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		// And one that never can. Without a horizon this is the loop that hangs.
		{"0 0 30 2 *", "2026-03-01 00:00", ""},
	}
	for _, c := range cases {
		s, err := ParseSchedule(c.expr)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", c.expr, err)
		}
		got, ok := s.Next(at(t, c.from))
		if c.want == "" {
			if ok {
				t.Errorf("%q.Next(%s) = %s, want no run at all", c.expr, c.from, got)
			}
			continue
		}
		if !ok {
			t.Errorf("%q.Next(%s) found nothing, want %s", c.expr, c.from, c.want)
			continue
		}
		if want := at(t, c.want); !got.Equal(want) {
			t.Errorf("%q.Next(%s) = %s, want %s", c.expr, c.from, got, want)
		}
	}
}

// Next must agree with Matches, which is what actually fires a job: a screen
// promising 04:10 while the scheduler wakes at 04:11 is worse than a blank
// column.
func TestScheduleNextIsAMinuteMatches(t *testing.T) {
	for _, expr := range []string{"* * * * *", "*/15 * * * *", "7,37 * * * *", "0 4 * * *", "0 9 * * 1-5"} {
		s, err := ParseSchedule(expr)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", expr, err)
		}
		now := at(t, "2026-06-20 13:37")
		for i := 0; i < 5; i++ {
			next, ok := s.Next(now)
			if !ok {
				t.Fatalf("%q ran out of runs after %d", expr, i)
			}
			if !s.Matches(next) {
				t.Errorf("%q.Next said %s, which Matches rejects", expr, next)
			}
			if !next.After(now) {
				t.Fatalf("%q.Next(%s) = %s, which is not later", expr, now, next)
			}
			now = next
		}
	}
}
