// Package session persists and restores conversations under the state dir.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
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
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// Save writes the session as <id>.json, refreshing its Updated timestamp.
func Save(s *Session, now time.Time) error {
	if s.ID == "" {
		return errors.New("session has no id")
	}
	d, err := dir()
	if err != nil {
		return err
	}
	s.Updated = now
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, s.ID+".json"), data, 0o644)
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

// Load reads a session by id.
func Load(id string) (*Session, error) {
	d, err := dir()
	if err != nil {
		return nil, err
	}
	return load(filepath.Join(d, id+".json"))
}

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
