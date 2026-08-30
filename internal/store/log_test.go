package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	awaitGroup(t, &wg, "every concurrent Append")

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

// A file the index cannot vouch for has to be read again, and neither half of
// that check is enough on its own.
//
// Same inode, same length, different mtime: an in-place rewrite. Different
// inode, same length, same mtime: a compaction, which renames a new file into
// place - and there size and mtime agree with the file that is gone, so only
// os.SameFile can tell. Missing either leaves the index holding the numbering of
// a file that no longer exists, and every cursor is answered against it.
func TestARewriteOfTheSameLengthIsNoticed(t *testing.T) {
	renumber := func(t *testing.T, data []byte) []byte {
		t.Helper()
		out := strings.Replace(string(data), `{"seq":1,`, `{"seq":5,`, 1)
		out = strings.Replace(out, `{"seq":2,`, `{"seq":6,`, 1)
		if len(out) != len(data) || out == string(data) {
			t.Fatalf("the rewrite has to be the same length and different; got %d vs %d bytes",
				len(out), len(data))
		}
		return []byte(out)
	}

	t.Run("in place", func(t *testing.T) {
		l := newTestLog(t)
		appendAll(t, l, "a", "b")
		if _, err := l.Range(0, 0); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(l.Path())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.Path(), renumber(t, data), 0o600); err != nil {
			t.Fatal(err)
		}
		// Asserted on the index rather than through Range: Range resyncs and
		// reads again on an error, so it answers correctly even when sync missed
		// the change - and then the test pins the retry instead of the check.
		if err := l.sync(); err != nil {
			t.Fatal(err)
		}
		if want := []int{5, 6}; !slices.Equal(l.seqs, want) {
			t.Errorf("the index holds %v after an in-place rewrite, want %v", l.seqs, want)
		}
	})

	t.Run("renamed into place with the same size and mtime", func(t *testing.T) {
		l := newTestLog(t)
		appendAll(t, l, "a", "b")
		if _, err := l.Range(0, 0); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(l.Path())
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(l.Path())
		if err != nil {
			t.Fatal(err)
		}
		replacement := l.Path() + ".replacement"
		if err := os.WriteFile(replacement, renumber(t, data), 0o600); err != nil {
			t.Fatal(err)
		}
		// The stat a compaction leaves behind can match the file it replaced on
		// both counts, so the test hands sync the hardest version of that.
		if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, l.Path()); err != nil {
			t.Fatal(err)
		}

		if err := l.sync(); err != nil {
			t.Fatal(err)
		}
		if want := []int{5, 6}; !slices.Equal(l.seqs, want) {
			t.Errorf("the index holds %v after the replacement, want %v; it went unnoticed",
				l.seqs, want)
		}
	})
}

// The last line of defence, and the only one that can act on a file that was
// replaced between the stat and the read: a record is checked against the
// sequence the index promised for it. Without it a stale offset that lands on a
// line boundary - which fixed-width records make systematic - comes back as a
// window of real records, shifted, and the client advances its cursor past the
// ones it never saw.
//
// The index is staled by hand because every path through Range is meant to stop
// this happening; what is under test is what remains when one of them does not.
func TestAReadFromAStaleIndexIsRefusedRatherThanShifted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.jsonl")
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	reader := NewLog[event](path)
	at := base
	reader.now = func() time.Time { return at }
	// Nine records, so every sequence is one digit and every timestamp the same
	// width: the offsets then shift by whole records.
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		at = at.Add(time.Second)
		if _, err := reader.Append(event{Kind: k}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reader.Range(0, 0); err != nil {
		t.Fatal(err)
	}
	staleSeqs := append([]int(nil), reader.seqs...)
	staleOffsets := append([]int64(nil), reader.offsets...)

	peer := NewLog[event](path)
	if dropped, err := peer.Compact(base.Add(3 * time.Second)); err != nil || dropped != 2 {
		t.Fatalf("the peer dropped %d records (%v), want 2", dropped, err)
	}

	// A limit, so the stale offsets stay inside the shortened file: without one
	// the window runs off the end and EOF answers for the check that is under
	// test, which passes whether or not the check is there.
	reader.seqs, reader.offsets = staleSeqs, staleOffsets
	got, err := reader.window(2, 2)
	if err == nil {
		t.Errorf("a read from a stale index returned %v instead of an error", got)
	} else if !strings.Contains(err.Error(), "the index gave") {
		t.Errorf("error = %v, want it to name the mismatch rather than the end of the file", err)
	}

	// And Range, which syncs first and reads again on an error, still answers
	// correctly.
	reader.seqs, reader.offsets = staleSeqs, staleOffsets
	window, err := reader.Range(2, 0)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(window) == 0 || window[0].Seq != 3 {
		t.Errorf("Range(2, 0) = %v, want the window starting at record 3", window)
	}
}

// A caller that cannot write unlocked has to be able to tell "come back later"
// from "this file is broken", without matching on the message.
func TestARefusedLockIsRecognisable(t *testing.T) {
	shrinkLockWait(t, 60*time.Millisecond)
	l := newTestLog(t)
	appendAll(t, l, "a")
	if err := os.WriteFile(l.Path()+".lock", []byte("a-live-peer"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := l.Append(event{Kind: "b"})
	if !errors.Is(err, ErrLocked) {
		t.Errorf("Append refused with %#v, want something errors.Is-able as ErrLocked", err)
	}
	if _, err := l.Compact(time.Now()); !errors.Is(err, ErrLocked) {
		t.Errorf("Compact refused with %#v, want ErrLocked", err)
	}
}

// The index is extended from where the last scan stopped rather than rebuilt,
// because rebuilding parses every record of the feed on every request that
// touches it. The guard on that shortcut is the numbering: a file that grew but
// is not the one the index describes cannot continue it.
func TestAFileThatGrewWithoutContinuingIsRefused(t *testing.T) {
	l := newTestLog(t)
	appendAll(t, l, "a", "b", "c")
	if _, err := l.Len(); err != nil {
		t.Fatal(err)
	}

	// Appended to the same inode, so the identity check passes and only the
	// numbering can catch it.
	f, err := os.OpenFile(l.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(Entry[event]{Seq: 1, At: time.Now().UTC(), V: event{Kind: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Len(); err == nil {
		t.Error("a file whose numbering restarted was indexed as a continuation")
	}
	// And the index was dropped rather than left half-built, so the next call
	// reads the file rather than a mixture of two.
	if _, err := l.Range(0, 0); err == nil {
		t.Error("the second call trusted an index built from a refused scan")
	}
}

// Only what is new is parsed when the file has merely grown. The visible half
// of that is the result being identical either way.
func TestAnIncrementalScanAgreesWithAFullOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.jsonl")
	warm := NewLog[event](path)
	appendAll(t, warm, "a", "b")
	if _, err := warm.Range(0, 0); err != nil {
		t.Fatal(err)
	}

	peer := NewLog[event](path)
	appendAll(t, peer, "c", "d")

	// warm's index is extended across the peer's two records; cold reads the
	// whole file from scratch.
	incremental, err := warm.Range(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	full, err := NewLog[event](path).Range(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(incremental) != len(full) {
		t.Fatalf("extended index sees %d records, a fresh one sees %d", len(incremental), len(full))
	}
	for i := range full {
		if incremental[i].Seq != full[i].Seq || incremental[i].V != full[i].V {
			t.Errorf("record %d: extended %+v, fresh %+v", i, incremental[i], full[i])
		}
	}
}

// Extending the index trusts the bytes before it unread, and the ordering guard
// only sees the record at the seam. A prefix renumbered in place under an inode
// that also grew would splice a stale head onto a fresh tail, and a cursor
// landing in the tail would then be answered with a short window and no error -
// the failure read's own check exists to stop, one level up.
func TestAnExtendedIndexNoticesARewrittenPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.jsonl")
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	warm := NewLog[event](path)
	warm.now = func() time.Time { return at }
	for _, k := range []string{"a", "b", "c"} {
		at = at.Add(time.Second)
		if _, err := warm.Append(event{Kind: k}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := warm.Range(0, 0); err != nil {
		t.Fatal(err)
	}

	// Renumbered 4, 5, 6 in place - same inode, same widths - and a seventh
	// record appended, so the file has grown and the seam is in order.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := string(data)
	for from, to := range map[string]string{`{"seq":1,`: `{"seq":4,`, `{"seq":2,`: `{"seq":5,`, `{"seq":3,`: `{"seq":6,`} {
		rewritten = strings.Replace(rewritten, from, to, 1)
	}
	if len(rewritten) != len(data) {
		t.Fatalf("the rewrite changed the length; the test needs it fixed")
	}
	seventh, err := json.Marshal(Entry[event]{Seq: 7, At: at, V: event{Kind: "g"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte(rewritten), append(seventh, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := warm.Range(3, 0)
	if err != nil {
		// Refusing is a fine answer too - what must not happen is a short window
		// returned as if it were the whole one.
		return
	}
	fresh, err := NewLog[event](path).Range(3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(fresh) {
		t.Errorf("the extended index answered with %d records and a fresh one with %d; "+
			"a cursor would advance past the difference", len(got), len(fresh))
	}
}
