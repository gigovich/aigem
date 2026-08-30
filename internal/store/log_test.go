package store

import (
	"encoding/json"
	"fmt"
	"math"
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
	// Two records, not one: Compact keeps the newest and returns without writing
	// when that is all there is, so a single-record log never reaches the rewrite
	// this test exists to check.
	appendAll(t, l, "a", "b")
	info, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the log is %o, want 600", perm)
	}
	// And still after a rewrite, which goes through a different path.
	dropped, err := l.Compact(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("Compact dropped %d records, so it never rewrote the file", dropped)
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

// An append writes at an offset rather than renaming, so two writers that both
// gave up on the lock pick the same sequence and the same offset: one record is
// accepted and then destroyed, and two clients are handed one cursor. File
// tolerates that fallback; Log must not.
func TestAppendFailsRatherThanWritingWithoutTheLock(t *testing.T) {
	shrinkLockWait(t, 60*time.Millisecond)
	l := newTestLog(t)
	appendAll(t, l, "a")

	// A lock file too young for breakStaleLock to reclaim: a peer that is alive
	// and mid-write, which is exactly when proceeding would destroy a record.
	lock := l.Path() + ".lock"
	if err := os.WriteFile(lock, []byte("a-live-peer"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Append(event{Kind: "b"}); err == nil {
		t.Fatal("an append proceeded without the lock")
	} else if !strings.Contains(err.Error(), "lock") {
		t.Errorf("error = %v, want it to name the lock", err)
	}
	// And nothing was written: the caller was told no, so the caller's retry is
	// the whole story.
	got, err := l.Range(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("the log holds %d records after a refused append, want 1", len(got))
	}

	// The peer goes away and the lock ages out; the retry lands.
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(event{Kind: "b"}); err != nil {
		t.Fatalf("the retry after the lock went stale failed: %v", err)
	}
}

// Compaction copies the survivors byte for byte. Decoding them through this
// binary's T and marshalling them back would strip whatever this binary does not
// know about - a field a newer version wrote, or one a rollback has forgotten -
// from records that were kept, silently and for good.
func TestCompactKeepsFieldsThisBinaryDoesNotKnowAbout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.jsonl")

	type richEvent struct {
		Kind  string `json:"kind"`
		Extra string `json:"extra"`
	}
	rich := NewLog[richEvent](path)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	at := base
	rich.now = func() time.Time { return at }
	for _, k := range []string{"a", "b", "c"} {
		if _, err := rich.Append(richEvent{Kind: k, Extra: "kept-" + k}); err != nil {
			t.Fatal(err)
		}
		at = at.Add(24 * time.Hour)
	}

	// A binary that knows only half the record trims the log.
	poor := NewLog[event](path)
	if _, err := poor.Compact(base.Add(24 * time.Hour)); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	got, err := rich.Range(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the log holds %d records, want 2", len(got))
	}
	for _, e := range got {
		if e.V.Extra != "kept-"+e.V.Kind {
			t.Errorf("record %d came back as %+v; the compaction stripped a field it could not read",
				e.Seq, e.V)
		}
	}
}

// A damaged line is not something to write past. The index is what an append
// truncates against, so building one out of a file this package cannot account
// for is how one bad line turns into lost records.
func TestADamagedRecordIsReportedByEveryMethod(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b", "c")

	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(data), "\n", 2)
	damaged := lines[0] + "\n{ this is not a record }\n"
	if err := os.WriteFile(l.Path(), []byte(damaged), 0o600); err != nil {
		t.Fatal(err)
	}

	next := NewLog[event](l.Path())
	if _, err := next.Range(0, 0); err == nil {
		t.Error("Range read a damaged log without complaining")
	}
	if _, err := next.Len(); err == nil {
		t.Error("Len counted a damaged log without complaining")
	}
	if _, err := next.Append(event{Kind: "d"}); err == nil {
		t.Error("Append wrote to a damaged log; the record it truncated against was not there")
	}
	if _, err := next.Compact(time.Now()); err == nil {
		t.Error("Compact rewrote a damaged log")
	}
}

// The sequences are read out of the file rather than derived from a position,
// so a file whose numbering has a gap - a line removed by hand - is refused
// rather than answered with a window shifted by the size of the gap.
func TestAGapInTheSequencesIsRefused(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b", "c")
	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")

	// Out of order, which a position-derived index cannot see at all.
	swapped := lines[1] + "\n" + lines[0] + "\n" + lines[2] + "\n"
	if err := os.WriteFile(l.Path(), []byte(swapped), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLog[event](l.Path()).Range(0, 0); err == nil {
		t.Error("a log whose records are out of order was read as if it were in order")
	}

	// A gap: record 2 removed. Reading from cursor 2 must still return record 3
	// and not a window computed from the missing one.
	gapped := lines[0] + "\n" + lines[2] + "\n"
	if err := os.WriteFile(l.Path(), []byte(gapped), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := NewLog[event](l.Path()).Range(2, 0)
	if err != nil {
		t.Fatalf("Range over a log with a gap: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 3 {
		t.Errorf("Range(2, 0) = %v, want only record 3", got)
	}
}

// The largest cursor a client could hold must not wrap round into the smallest
// and hand it the whole log back.
func TestAnEnormousCursorReturnsNothing(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b")
	for _, since := range []int{math.MaxInt, math.MaxInt - 1, 99} {
		got, err := l.Range(since, 0)
		if err != nil {
			t.Fatalf("Range(%d, 0): %v", since, err)
		}
		if len(got) != 0 {
			t.Errorf("Range(%d, 0) returned %d records", since, len(got))
		}
	}
	// And a nonsense cursor from the other end is the whole log, not a panic.
	got, err := l.Range(math.MinInt, 0)
	if err != nil {
		t.Fatalf("Range(MinInt, 0): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("Range(MinInt, 0) returned %d records, want 2", len(got))
	}
}

// The mtime half of the staleness check. A peer's compaction that lands on the
// same byte count is the case it exists for; with size alone the index keeps the
// sequences of the file that is gone, and every cursor is then answered against
// numbering that no longer exists.
func TestASameSizeRewriteIsNoticed(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b")
	if _, err := l.Range(0, 0); err != nil {
		t.Fatal(err)
	}

	// Renumbered 5 and 6, which is the same number of bytes: what a peer rewrite
	// can look like from a stat alone.
	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	renumbered := strings.Replace(string(data), `{"seq":1,`, `{"seq":5,`, 1)
	renumbered = strings.Replace(renumbered, `{"seq":2,`, `{"seq":6,`, 1)
	if len(renumbered) != len(data) || renumbered == string(data) {
		t.Fatalf("the rewrite has to be the same length and different; got %d vs %d bytes",
			len(renumbered), len(data))
	}
	if err := os.WriteFile(l.Path(), []byte(renumbered), 0o600); err != nil {
		t.Fatal(err)
	}

	// Both records are past a cursor of 1 now. An index still holding 1 and 2
	// would answer with one.
	got, err := l.Range(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Range(1, 0) returned %d records, want 2; a same-size rewrite went unnoticed", len(got))
	}
}
