// Package session persists and restores conversations under the state dir.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/store"
)

// Meta describes a saved session without its full message history.
type Meta struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Model is the provider/id reference the session was using, restored on
	// resume. Omitempty keeps older sessions and the local-only path unaffected.
	Model string `json:"model,omitempty"`
}

// Session is the persisted unit: metadata plus the full message history.
type Session struct {
	Meta
	Messages []llm.Message `json:"messages"`
}

// NewID derives a sortable, filesystem-safe id from a timestamp, with enough
// randomness that two conversations started in the same second are still two.
// A second was fine while a session was one file written whole; the event
// journal is opened for append, so a collision no longer overwrites - it
// interleaves two conversations under one set of sequence numbers.
func NewID(now time.Time) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Entropy failure is not a reason to refuse to start a conversation; the
		// timestamp alone is what this always used to be.
		return now.UTC().Format("20060102-150405")
	}
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func dir() (string, error) {
	base, err := config.StateDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "sessions")
	// 0700, like the state directory above it: a session holds the whole
	// conversation, including whatever the agent read out of the repository.
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves an existing directory's mode alone, so an installation
	// made before this would have stayed 0755 for good. Narrowing is
	// best-effort: a directory this cannot chmod still holds 0600 files, and
	// refusing to save over it would help nobody.
	if info, err := os.Stat(d); err == nil && info.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(d, 0o700)
	}
	return d, nil
}

// pathFor is the one place an id becomes a path, so it is the one place that
// has to refuse an id that is not a name. NewID makes them, but Load takes one
// from whoever is asking - and the browser daemon will be asking - where
// "../auth.json" is a path rather than a session.
func pathFor(id string) (string, error) {
	if id == "" || id == "." || id == ".." || id != filepath.Base(id) {
		return "", fmt.Errorf("session %q: not a session id", id)
	}
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, id+".json"), nil
}

// Save writes the session as <id>.json, refreshing its Updated timestamp.
//
// It goes through internal/store, which writes a temp file and renames it, at
// 0600 rather than 0644 - a conversation is not world-readable state.
//
// The rename is what matters. os.WriteFile truncates and then writes, so anyone
// reading the file in that window - a browser tab on the same conversation, a
// second aigem, List walking the directory - saw something that was not JSON,
// and a process killed there left that state on disk. A reader now sees the
// previous session or this one.
func Save(s *Session, now time.Time) error {
	if s.ID == "" {
		return errors.New("session has no id")
	}
	path, err := pathFor(s.ID)
	if err != nil {
		return err
	}
	s.Updated = now
	return store.New[Session](path).Save(*s)
}

// List returns saved sessions, most recently updated first.
func List() ([]Meta, error) {
	d, err := dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	var metas []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// Skip pre-compaction backups (<id>.precompact-<n>.json); they are raw
		// message arrays, not sessions, and must not appear in the resume list.
		if strings.Contains(e.Name(), ".precompact-") {
			continue
		}
		s, err := load(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		metas = append(metas, s.Meta)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Updated.After(metas[j].Updated) })
	return metas, nil
}

// Load reads a session by id, and reports one that is not there as an error.
//
// The document has to agree with the file it was found in. Callers take the id
// out of what comes back - internal/uisession names the event journal's
// directory after it - so validating the id that was asked for and then handing
// back a different one would leave the second conversion unguarded, and a
// document claiming "../.." would put the journal outside the state directory.
// Nothing this package writes can disagree: Save names the file after the id.
func Load(id string) (*Session, error) {
	path, err := pathFor(id)
	if err != nil {
		return nil, err
	}
	s, err := load(path)
	if err != nil {
		return nil, err
	}
	if s.ID != id {
		return nil, fmt.Errorf("session %q: the document in it calls itself %q", id, s.ID)
	}
	return s, nil
}

// load reads the document directly rather than through the store, which answers
// a missing file with the zero value. Resuming a session that does not exist has
// to say so, not hand back an empty conversation under the id that was asked
// for. Reading uncoordinated is safe because every write lands by rename.
func load(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Title derives a short title from the first user message.
func Title(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	if s == "" {
		return "(untitled)"
	}
	return s
}
