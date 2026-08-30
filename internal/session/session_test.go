package session

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/llm"
)

func TestSaveListLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	s := &Session{
		Meta:     Meta{ID: NewID(now), Title: Title("hello world"), Created: now, Model: "openai/gpt-5.6-sol"},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello world"}},
	}
	if err := Save(s, now); err != nil {
		t.Fatal(err)
	}

	metas, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Title != "hello world" {
		t.Fatalf("unexpected metas: %+v", metas)
	}
	if metas[0].Model != "openai/gpt-5.6-sol" {
		t.Fatalf("model not persisted in meta: %q", metas[0].Model)
	}

	loaded, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "hello world" {
		t.Fatalf("unexpected messages: %+v", loaded.Messages)
	}
}

func TestTitle(t *testing.T) {
	if got := Title("ab\ncd"); got != "ab cd" {
		t.Fatalf("newline not flattened: %q", got)
	}
	if Title("") != "(untitled)" {
		t.Fatal("empty title should be (untitled)")
	}
}

func saved(t *testing.T, id string) string {
	t.Helper()
	base := os.Getenv("XDG_STATE_HOME")
	return filepath.Join(base, "aigem", "sessions", id+".json")
}

// A conversation holds whatever the agent read out of the repository. It was
// written 0644 into a 0755 directory, which on a shared machine is every other
// account's to read.
func TestASavedSessionIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()
	s := &Session{Meta: Meta{ID: NewID(now)}}
	if err := Save(s, now); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(saved(t, s.ID))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the session file is %o, want 600", perm)
	}
	dir, err := os.Stat(filepath.Dir(saved(t, s.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("the sessions directory is %o, want 700", perm)
	}
}

// The runner holds live sessions that save on a turn, on Close and on a
// removal, while a browser tab is reading the same file. os.WriteFile truncates
// and then writes, so a reader in that window got something that was not JSON -
// about 600 times in 2000 saves, measured - and a process killed there left
// that on disk. A save that lands by rename cannot be caught halfway, which
// this pins down without racing anything: a reader holding the file open goes
// on reading the document it opened.
func TestAReaderHoldingTheFileIsUnaffectedByASave(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a rename onto an open file is refused there, and the store retries instead")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()
	id := NewID(now)

	long := make([]llm.Message, 400)
	for i := range long {
		long[i] = llm.Message{Role: llm.RoleUser, Content: strings.Repeat("x", 200)}
	}
	if err := Save(&Session{Meta: Meta{ID: id}, Messages: long}, now); err != nil {
		t.Fatal(err)
	}

	open, err := os.Open(saved(t, id))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = open.Close() }()

	// A much shorter history, so a truncate-in-place is unmistakable.
	if err := Save(&Session{Meta: Meta{ID: id}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(open)
	if err != nil {
		t.Fatal(err)
	}
	var got Session
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("the reader's document stopped parsing partway through a save: %v", err)
	}
	if len(got.Messages) != len(long) {
		t.Errorf("the reader saw %d messages, want the %d it opened the file on",
			len(got.Messages), len(long))
	}

	// And the save did land, for whoever opens it next.
	next, err := Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Messages) != 0 {
		t.Errorf("the new document holds %d messages, want none", len(next.Messages))
	}
}

// An id becomes a path, and the browser daemon takes ids from requests. Nothing
// stopped "../auth" from naming a file outside the sessions directory - so the
// decoy below is a real, loadable document one level up, which Load has to
// refuse rather than read.
func TestAnIdThatIsAPathIsRefused(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	now := time.Now()
	// Creates the sessions directory, so the traversal below has somewhere to
	// climb out of.
	if err := Save(&Session{Meta: Meta{ID: NewID(now)}}, now); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(state, "aigem", "secret.json")
	body, err := json.Marshal(Session{Meta: Meta{ID: "secret", Title: "not a session"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decoy, body, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"../secret", "..", ".", "a/b", string(filepath.Separator) + "etc"} {
		if got, err := Load(id); err == nil {
			t.Errorf("Load(%q) returned %+v", id, got.Meta)
		}
		if err := Save(&Session{Meta: Meta{ID: id}}, time.Now()); err == nil {
			t.Errorf("Save(%q) was allowed", id)
		}
	}

	// The decoy is untouched: no write escaped either.
	after, err := os.ReadFile(decoy)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(body) {
		t.Error("a save with a traversing id overwrote a file outside the sessions directory")
	}
}

// Resuming a session that is not there has to say so. The store answers a
// missing document with the zero value, which here would be an empty
// conversation handed back under the id that was asked for.
func TestLoadingAMissingSessionIsAnError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := Load("20260830-150405-abcdef"); err == nil {
		t.Error("a session that was never saved loaded without an error")
	}
}

// The store keeps a lock file and, mid-write, a temp beside the document. Both
// live in the directory List walks.
func TestListIgnoresWhatTheStoreLeavesBeside(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()
	s := &Session{Meta: Meta{ID: NewID(now), Title: "real"}}
	if err := Save(s, now); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(saved(t, s.ID))
	for _, name := range []string{
		s.ID + ".json.lock",
		s.ID + "-123456.json.tmp",
		s.ID + ".precompact-1.json",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	metas, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Title != "real" {
		t.Fatalf("List returned %+v, want the one real session", metas)
	}
}
