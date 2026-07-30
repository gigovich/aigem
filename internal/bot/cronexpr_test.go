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
