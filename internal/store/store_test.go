package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type doc struct {
	Version int            `json:"version"`
	Counts  map[string]int `json:"counts"`
}

func TestLoadMissingReturnsZero(t *testing.T) {
	s := New[doc](filepath.Join(t.TempDir(), "nope.json"))
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != 0 || got.Counts != nil {
		t.Fatalf("Load of a missing file = %+v, want zero value", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := New[doc](filepath.Join(t.TempDir(), "sub", "doc.json"))
	want := doc{Version: 3, Counts: map[string]int{"a": 1}}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != want.Version || got.Counts["a"] != 1 {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// The document can hold credentials-adjacent state, so it must not be
// world-readable even when the process umask is permissive.
func TestSaveIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "doc.json")
	s := New[doc](path)
	if err := s.Save(doc{Version: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %04o, want 0600", perm)
	}
}

func TestLoadRejectsCorruptDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New[doc](path).Load(); err == nil {
		t.Fatal("Load of a corrupt document succeeded, want error")
	}
}

func TestUpdateStartsFromZeroValue(t *testing.T) {
	s := New[doc](filepath.Join(t.TempDir(), "doc.json"))
	err := s.Update(func(d *doc) error {
		if d.Version != 0 {
			t.Errorf("Update saw version %d, want 0", d.Version)
		}
		d.Version = 7
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != 7 {
		t.Fatalf("version = %d, want 7", got.Version)
	}
}

// An Update that fails must leave the previous document intact, since the
// caller's next Load has to see a state it can reason about.
func TestUpdateDoesNotSaveOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	s := New[doc](path)
	if err := s.Save(doc{Version: 1}); err != nil {
		t.Fatal(err)
	}
	wantErr := os.ErrInvalid
	if err := s.Update(func(d *doc) error { d.Version = 99; return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("Update err = %v, want %v", err, wantErr)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("version = %d, want the document untouched at 1", got.Version)
	}
}

// Concurrent writers are the reason the lock exists: every increment has to
// survive, which only holds if load-modify-save is serialized.
func TestConcurrentUpdatesAllLand(t *testing.T) {
	s := New[doc](filepath.Join(t.TempDir(), "doc.json"))
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Update(func(d *doc) error {
				if d.Counts == nil {
					d.Counts = map[string]int{}
				}
				d.Counts["n"]++
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Counts["n"] != writers {
		t.Fatalf("counts[n] = %d, want %d", got.Counts["n"], writers)
	}
}

func TestSaveSweepsOrphanTempsButKeepsRecentOnes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.json")
	s := New[doc](path)

	stale := filepath.Join(dir, "doc-123456"+tempSuffix)
	fresh := filepath.Join(dir, "doc-999999"+tempSuffix)
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * tempOrphan)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if err := s.Save(doc{Version: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a temp older than tempOrphan survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent temp was swept; another process may still be writing it")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the document itself is missing: %v", err)
	}
}

// The sweep now looks at every name in the directory, so what keeps real
// documents safe is the .json.tmp suffix and CreateTemp's digit shape - nothing
// else. A store at <state>/project.json must not eat <state>/project-trust.json,
// which is every capability approval the user has ever given, deleted silently
// an hour after it was written.
func TestSaveDoesNotSweepASiblingDocumentSharingTheStem(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * tempOrphan)
	siblings := []string{
		filepath.Join(dir, "doc-trust.json"),   // another store, same stem
		filepath.Join(dir, "doc-2026-08.json"), // digits, but not CreateTemp's shape
		filepath.Join(dir, "doc-archive.json"),
		filepath.Join(dir, "unrelated-1.json"),
		filepath.Join(dir, "doc-123456.txt"), // right shape, wrong extension
		// The dangerous one: a real document whose name is exactly what a temp
		// would look like if temps ended in .json. Numbered documents are an
		// obvious thing to keep beside an index.
		filepath.Join(dir, "doc-7.json"),
		filepath.Join(dir, "doc-123456.json"),
	}
	for _, p := range siblings {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := New[doc](filepath.Join(dir, "doc.json")).Save(doc{Version: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, p := range siblings {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the sweep deleted %s, which is not one of its temp files", filepath.Base(p))
		}
	}
}

// A lock left by a killed process must not wedge the store forever.
//
// lockWait is pushed out of reach first: the unlocked fallback would otherwise
// complete this update on its own, and the test would pass with the whole
// stale-lock path deleted. Whether the lock was broken or merely waited out is
// visible afterwards - breaking it takes and releases our own, leaving nothing
// behind; the fallback leaves the dead one in place.
func TestUpdateBreaksStaleLock(t *testing.T) {
	shrinkLockWait(t, time.Hour)
	path := filepath.Join(t.TempDir(), "doc.json")
	s := New[doc](path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	lock := path + ".lock"
	if err := os.WriteFile(lock, []byte("a-dead-process"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Update(func(d *doc) error { d.Version = 5; return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Update blocked on a stale lock")
	}

	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("the stale lock is still there; the update waited it out rather than breaking it")
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 5 {
		t.Fatalf("version = %d, want 5", got.Version)
	}
}

// A live lock held by another process is waited on, then given up on: a peer
// that hangs must not be able to wedge this one. The update goes through
// unlocked, and the peer's lock is left alone.
func TestUpdateProceedsWithoutALockHeldTooLongByAPeer(t *testing.T) {
	shrinkLockWait(t, 60*time.Millisecond)
	path := filepath.Join(t.TempDir(), "doc.json")
	s := New[doc](path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	lock := path + ".lock"
	if err := os.WriteFile(lock, []byte("a-live-peer"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Update(func(d *doc) error { d.Version = 9; return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 9 {
		t.Fatalf("version = %d, want 9 - the update never happened", got.Version)
	}
	data, err := os.ReadFile(lock)
	if err != nil {
		t.Fatalf("the peer's lock was removed: %v", err)
	}
	if string(data) != "a-live-peer" {
		t.Errorf("lock = %q, want the peer's own token untouched", data)
	}
}

// Windows refuses to rename onto a path another handle has open, and CI never
// runs there - so the retry is reachable only through the seam.
func TestReplaceRetriesUntilTheTargetIsFree(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "tmp"), filepath.Join(dir, "doc.json")
	if err := os.WriteFile(from, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	prev := renameFile
	t.Cleanup(func() { renameFile = prev })
	attempts := 0
	renameFile = func(a, b string) error {
		attempts++
		if attempts < 3 {
			return os.ErrPermission
		}
		return prev(a, b)
	}

	if err := replaceRetrying(from, to, true); err != nil {
		t.Fatalf("replaceRetrying: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want it to retry until the rename took", attempts)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "new" {
		t.Errorf("target = %q, %v; want the renamed contents", data, err)
	}
}

func TestReplaceDoesNotRetryWhenRetryingIsOff(t *testing.T) {
	prev := renameFile
	t.Cleanup(func() { renameFile = prev })
	attempts := 0
	renameFile = func(string, string) error { attempts++; return os.ErrPermission }

	if err := replaceRetrying("a", "b", false); err == nil {
		t.Fatal("replaceRetrying reported success for a rename that always fails")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 - a real failure must not be delayed", attempts)
	}
}

// A path with no usable stem still has to produce a pattern CreateTemp accepts,
// or the store cannot write at all.
func TestDegenerateNamesStillSave(t *testing.T) {
	for _, name := range []string{"doc", ".json", "a.b.json", "..json", "doc.JSON"} {
		s := New[doc](filepath.Join(t.TempDir(), name))
		if err := s.Save(doc{Version: 1}); err != nil {
			t.Errorf("Save to %q: %v", name, err)
			continue
		}
		if s.Path() == "" {
			t.Errorf("Path() is empty for %q", name)
		}
		got, err := s.Load()
		if err != nil || got.Version != 1 {
			t.Errorf("round trip through %q = %+v, %v", name, got, err)
		}
	}
}

// Indented and newline-terminated because these documents are read and edited
// by hand when something has gone wrong.
func TestSaveWritesIndentedJSONWithTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	if err := New[doc](path).Save(doc{Version: 1, Counts: map[string]int{"a": 2}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Error("document does not end with a newline")
	}
	if !strings.Contains(string(data), "\n  \"version\": 1") {
		t.Errorf("document is not indented:\n%s", data)
	}
	var back doc
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("document is not valid JSON: %v", err)
	}
}

// shrinkLockWait makes the fallback reachable in a test. The file lock gives up
// after lockWait and proceeds unlocked so a peer killed mid-write cannot wedge
// the daemon, and that fallback is the only path on which two goroutines can
// overlap - a body shorter than the real two seconds never reaches it, and a
// test that waits two seconds per case is a test nobody runs.
func shrinkLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := lockWait
	lockWait = d
	t.Cleanup(func() { lockWait = prev })
}

// Between goroutines the unlocked fallback is a silently lost update - and
// concurrent request handlers on one document are exactly that case - so an
// in-process mutex has to keep two update bodies from ever overlapping.
func TestUpdateBodiesNeverOverlap(t *testing.T) {
	shrinkLockWait(t, 30*time.Millisecond)
	s := New[doc](filepath.Join(t.TempDir(), "doc.json"))
	var inside atomic.Int32
	var overlapped atomic.Bool

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Update(func(d *doc) error {
				if inside.Add(1) != 1 {
					overlapped.Store(true)
				}
				// Longer than lockWait, so a waiter that is not held by the mutex
				// reaches the unlocked fallback and runs alongside this body.
				time.Sleep(120 * time.Millisecond)
				inside.Add(-1)
				d.Version++
				return nil
			})
		}()
	}
	wg.Wait()

	if overlapped.Load() {
		t.Error("two Update bodies ran at once; one of their writes is lost")
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 6 {
		t.Fatalf("version = %d, want 6 - an update was lost", got.Version)
	}
}

// Save is a last-write-wins replace, but it must still not interleave with an
// Update's read-modify-write, or the Update reads a value Save is replacing.
//
// The handshake replaces a sleep: Save is not started until the Update body is
// provably running, so there is no timing on which this test asserts nothing.
func TestSaveIsSerializedAgainstUpdate(t *testing.T) {
	shrinkLockWait(t, 30*time.Millisecond)
	s := New[doc](filepath.Join(t.TempDir(), "doc.json"))
	running := make(chan struct{})
	var inside atomic.Int32
	var overlapped atomic.Bool

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = s.Update(func(d *doc) error {
			inside.Add(1)
			close(running)
			time.Sleep(120 * time.Millisecond)
			inside.Add(-1)
			d.Version = 1
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		<-running
		// Save holds no body of its own, so the overlap is observed by whether it
		// returns while the Update body is still inside.
		_ = s.Save(doc{Version: 2})
		if inside.Load() != 0 {
			overlapped.Store(true)
		}
	}()
	wg.Wait()

	if overlapped.Load() {
		t.Error("Save returned while an Update body was still running")
	}
}

// One sweep collects every orphan in the directory, whichever store abandoned
// it. Keying the predicate on the calling store's stem meant keying the flag on
// its document too - one map entry per document for the life of the process,
// which for a store per session grows without bound.
func TestASweepCollectsAnotherStoresOrphanInTheSameDirectory(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "ticket-123456"+tempSuffix)
	if err := os.WriteFile(orphan, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * tempOrphan)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	// The project store alone, and never the one whose stem the orphan carries.
	if err := New[doc](filepath.Join(dir, "project.json")).Save(doc{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("a store left a neighbour's orphan behind; nothing else will ever collect it")
	}
}

// isTempName is coupled to the shape os.CreateTemp produces, which the standard
// library documents only as "a random string". A sweep that stops matching is
// as silent as one that matches too much.
func TestIsTempNameMatchesWhatCreateTempActuallyProduces(t *testing.T) {
	dir := t.TempDir()
	f := New[doc](filepath.Join(dir, "doc.json"))
	for range 200 {
		tmp, err := os.CreateTemp(dir, f.prefix)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(tmp.Name())
		tmp.Close()
		os.Remove(tmp.Name())
		if !isTempName(name) {
			t.Fatalf("isTempName(%q) = false; the sweep would leave this orphan behind", name)
		}
	}
}

// A holder that overran lockStale has its lock broken and replaced. Removing
// the replacement on the way out would let a third writer in alongside its
// owner, so a release only removes a lock that still carries its own token.
func TestReleaseLeavesALockThatIsNoLongerOurs(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "doc.json.lock")
	if err := os.WriteFile(lock, []byte("someone-else"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseLock(lock, "ours")
	if _, err := os.Stat(lock); err != nil {
		t.Error("release removed a lock carrying another writer's token")
	}

	if err := os.WriteFile(lock, []byte("ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseLock(lock, "ours")
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Error("release left our own lock behind")
	}
}

// A lock that cannot be read is left alone: lockStale reclaims it, and guessing
// is what the token check exists to prevent.
func TestReleaseLeavesAnUnreadableLockAlone(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permission bits and a non-root user")
	}
	lock := filepath.Join(t.TempDir(), "doc.json.lock")
	if err := os.WriteFile(lock, []byte("someone-else"), 0o000); err != nil {
		t.Fatal(err)
	}
	releaseLock(lock, "ours")
	if _, err := os.Stat(lock); err != nil {
		t.Error("release removed a lock it could not read")
	}
}

// The lock loop used to loop straight back on a break that could not unlink,
// skipping both the sleep and the give-up test: a core spun at 100% forever
// while holding the in-process mutex, so every other writer of the document
// blocked with it. A read-only directory is the everyday shape of that - a
// state dir owned by another uid, a read-only mount, a peer's lock under a
// sticky bit.
func TestUpdateGivesUpWhenAStaleLockCannotBeRemoved(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permission bits and a non-root user")
	}
	shrinkLockWait(t, 60*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.json")
	lock := path + ".lock"
	if err := os.WriteFile(lock, []byte("a-dead-process"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		// The save cannot succeed in a read-only directory; returning at all is
		// what this asserts.
		_ = New[doc](path).Update(func(d *doc) error { d.Version = 1; return nil })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Update never returned; the lock loop is spinning on a break it cannot complete")
	}
}

func TestDeleteRemovesTheDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	f := New[doc](path)
	if err := f.Save(doc{Version: 7}); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the document is still on disk")
	}
	// And it reads back as the zero value, exactly like one that was never
	// written - which is what lets a caller delete without a second code path.
	got, err := f.Load()
	if err != nil {
		t.Fatalf("Load after Delete: %v", err)
	}
	if got.Version != 0 {
		t.Errorf("Load after Delete = %+v, want the zero value", got)
	}
}

// A caller asking for the document to be gone is asking for a state, not for an
// event: removing one twice, or one that a peer removed first, is that state.
func TestDeletingWhatIsNotThereIsNotAnError(t *testing.T) {
	f := New[doc](filepath.Join(t.TempDir(), "doc.json"))
	if err := f.Delete(); err != nil {
		t.Fatalf("Delete on a missing document: %v", err)
	}
	if err := f.Delete(); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

// The map of per-document mutexes used to grow for the life of the process. One
// entry is nothing; one per session held by a daemon that runs for weeks is a
// leak whose size the workload chooses.
func TestThePathMutexTableDoesNotGrow(t *testing.T) {
	dir := t.TempDir()
	for i := range 200 {
		f := New[doc](filepath.Join(dir, fmt.Sprintf("doc-%03d.json", i)))
		if err := f.Save(doc{Version: i}); err != nil {
			t.Fatal(err)
		}
		if err := f.Delete(); err != nil {
			t.Fatal(err)
		}
	}
	pathsMu.Lock()
	held := len(paths)
	pathsMu.Unlock()
	if held != 0 {
		t.Errorf("the store holds %d path mutexes after every caller released them", held)
	}
}

// The entry may only go when the last caller leaves it. Dropping it while a
// goroutine is still waiting would hand the next caller a second mutex for the
// same document, which is no mutex at all - and this is the test that fails if
// the count is moved after the Lock.
func TestAWaiterKeepsThePathMutexAlive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	first := lockPath(path)

	waiting := make(chan func(), 1)
	go func() { waiting <- lockPath(path) }()

	// The waiter is parked on the mutex; the entry has to still be there, and
	// there has to be exactly one.
	for {
		pathsMu.Lock()
		h, ok := paths[canonical(path)]
		refs := 0
		if ok {
			refs = h.refs
		}
		pathsMu.Unlock()
		if refs == 2 {
			break
		}
		if !ok {
			t.Fatal("the entry was dropped while a caller was waiting on it")
		}
		time.Sleep(time.Millisecond)
	}

	first()
	second := <-waiting
	second()

	pathsMu.Lock()
	_, still := paths[canonical(path)]
	pathsMu.Unlock()
	if still {
		t.Error("the entry outlived every caller")
	}
}
