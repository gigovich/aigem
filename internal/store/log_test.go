package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type event struct {
	Kind string `json:"kind"`
}

func newTestLog(t *testing.T) *Log[event] {
	t.Helper()
	return NewLog[event](filepath.Join(t.TempDir(), "activity.jsonl"))
}

func appendAll(t *testing.T, l *Log[event], kinds ...string) []Entry[event] {
	t.Helper()
	var out []Entry[event]
	for _, k := range kinds {
		e, err := l.Append(event{Kind: k})
		if err != nil {
			t.Fatalf("Append(%q): %v", k, err)
		}
		out = append(out, e)
	}
	return out
}

func TestAppendNumbersFromOne(t *testing.T) {
	l := newTestLog(t)
	got := appendAll(t, l, "a", "b", "c")
	for i, e := range got {
		if e.Seq != i+1 {
			t.Errorf("record %d has sequence %d", i, e.Seq)
		}
		if e.At.IsZero() {
			t.Errorf("record %d carries no time", i)
		}
	}
}

// A log that was never written is empty, not an error: the daemon reads the
// feed before anything has happened on every cold start.
func TestRangeOnAMissingLogIsEmpty(t *testing.T) {
	l := newTestLog(t)
	got, err := l.Range(0, 0)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Range = %v, want nothing", got)
	}
	if n, err := l.Len(); err != nil || n != 0 {
		t.Errorf("Len = %d, %v; want 0, nil", n, err)
	}
}

func TestRangeResumesFromACursor(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b", "c", "d")

	got, err := l.Range(2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].V.Kind != "c" || got[1].V.Kind != "d" {
		t.Fatalf("Range(2, 0) = %v, want c and d", got)
	}
	// The cursor at the end yields nothing, which is what a client polling an
	// idle feed gets on every call.
	if tail, err := l.Range(4, 0); err != nil || len(tail) != 0 {
		t.Errorf("Range(4, 0) = %v, %v; want nothing", tail, err)
	}
}

func TestRangeHonoursTheLimit(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b", "c", "d")

	got, err := l.Range(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].V.Kind != "a" || got[1].V.Kind != "b" {
		t.Fatalf("Range(0, 2) = %v, want the first two", got)
	}
}

// The index is what makes a resume a seek rather than a scan, and it has to be
// rebuilt from the file by a process that did not write it.
func TestASecondLogReadsWhatTheFirstWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.jsonl")
	first := NewLog[event](path)
	appendAll(t, first, "a", "b", "c")

	second := NewLog[event](path)
	got, err := second.Range(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 3 {
		t.Fatalf("a fresh log read %v, want records 2 and 3", got)
	}
}

// Two logs on one path are two indexes, and an append through one has to be
// visible to the other - otherwise the second would hand its next record a
// sequence the first has already used.
func TestAnAppendThroughOneLogIsSeenByTheOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.jsonl")
	first := NewLog[event](path)
	second := NewLog[event](path)
	appendAll(t, first, "a")
	appendAll(t, second, "b")
	appendAll(t, first, "c")

	got, err := first.Range(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("the log holds %d records, want 3: %v", len(got), got)
	}
	for i, e := range got {
		if e.Seq != i+1 {
			t.Errorf("record %d has sequence %d; a sequence was reused or skipped", i, e.Seq)
		}
	}
}

func TestConcurrentAppendsAllLandWithDistinctSequences(t *testing.T) {
	l := newTestLog(t)
	const writers = 24
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Append(event{Kind: fmt.Sprintf("e%02d", i)}); err != nil {
				t.Errorf("Append: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := l.Range(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != writers {
		t.Fatalf("the log holds %d records, want %d", len(got), writers)
	}
	seen := map[int]bool{}
	for _, e := range got {
		if seen[e.Seq] {
			t.Errorf("sequence %d was handed out twice", e.Seq)
		}
		seen[e.Seq] = true
	}
}

// A process killed midway through a write leaves a line with no newline. The
// next append has to overwrite it: a log that parsed it would be unreadable
// from that point on, for good.
func TestATornTailIsOverwrittenByTheNextAppend(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a")

	f, err := os.OpenFile(l.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":2,"at":"2026-08-30T00:00`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh index, because the point is what a restarted daemon does with it.
	next := NewLog[event](l.Path())
	e, err := next.Append(event{Kind: "b"})
	if err != nil {
		t.Fatalf("Append after a torn write: %v", err)
	}
	if e.Seq != 2 {
		t.Errorf("the record after a torn one has sequence %d, want 2", e.Seq)
	}
	got, err := next.Range(0, 0)
	if err != nil {
		t.Fatalf("Range after a torn write: %v", err)
	}
	if len(got) != 2 || got[1].V.Kind != "b" {
		t.Fatalf("the log reads back as %v, want a and b", got)
	}
}

func TestCompactDropsWhatIsOlderThanTheCutoff(t *testing.T) {
	l := newTestLog(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	at := base
	l.now = func() time.Time { return at }
	for _, k := range []string{"a", "b", "c", "d"} {
		appendAll(t, l, k)
		at = at.Add(24 * time.Hour)
	}

	dropped, err := l.Compact(base.Add(48 * time.Hour))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if dropped != 2 {
		t.Fatalf("Compact dropped %d records, want 2", dropped)
	}
	got, err := l.Range(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].V.Kind != "c" {
		t.Fatalf("the log holds %v, want c and d", got)
	}
	// The whole point of the wrapper: a client holding 3 as its cursor still
	// gets record 4 and nothing else, even though 3 is now the first line.
	if got[0].Seq != 3 || got[1].Seq != 4 {
		t.Errorf("compaction renumbered the survivors: %v", got)
	}
	resumed, err := l.Range(3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0].Seq != 4 {
		t.Errorf("a cursor from before the compaction resumed at %v, want record 4", resumed)
	}
}

// Emptying the log would restart the numbering at 1, and the next record would
// then carry a sequence a client is still holding - a resume that replays a
// different history without saying so.
func TestCompactKeepsTheNewestRecordEvenWhenItIsOlderThanTheCutoff(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b")

	dropped, err := l.Compact(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("Compact dropped %d records, want 1", dropped)
	}
	next, err := l.Append(event{Kind: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if next.Seq != 3 {
		t.Errorf("the record after a full compaction has sequence %d, want 3", next.Seq)
	}
}

func TestCompactDoesNothingWhenEverythingIsRecent(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b")
	before, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}

	dropped, err := l.Compact(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if dropped != 0 {
		t.Errorf("Compact dropped %d records from a log with nothing old in it", dropped)
	}
	after, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the log was rewritten with nothing to drop")
	}
}

// The records are the same class of state as everything else this package
// keeps, and a state directory is not world-readable.
func TestTheLogIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	l := newTestLog(t)
	appendAll(t, l, "a")
	info, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the log is %o, want 600", perm)
	}
	// And still after a rewrite, which goes through a different path.
	if _, err := l.Compact(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the compacted log is %o, want 600", perm)
	}
}

// The file is read by people when something has gone wrong, so it has to stay
// one record per line rather than becoming an opaque blob.
func TestTheFileIsOneJSONObjectPerLine(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b")
	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the file holds %d lines for two records", len(lines))
	}
	for i, line := range lines {
		var e Entry[event]
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Errorf("line %d does not parse on its own: %v", i+1, err)
		}
	}
}

// A file that is not this log's is a failure to report, not something to
// silently treat as empty and then overwrite.
func TestACorruptLogIsAnError(t *testing.T) {
	l := newTestLog(t)
	if err := os.MkdirAll(filepath.Dir(l.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Path(), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Range(0, 0); err == nil {
		t.Error("a log whose first record does not parse was read as if it were fine")
	}
}
