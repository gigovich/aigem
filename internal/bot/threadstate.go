package bot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/gigovich/aigem/internal/config"
)

// ThreadStore persists the set of chat threads a bot follows, so thread membership survives a
// restart.
type ThreadStore struct {
	path string

	mu   sync.Mutex
	seen uint64 // highest version written, so a slow writer cannot overwrite a newer set
}

// NewThreadStore returns the store for a bot: ~/.local/state/aigem/threads-<name>.json.
func NewThreadStore(botName string) (*ThreadStore, error) {
	dir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	return &ThreadStore{path: filepath.Join(dir, "threads-"+botName+".json")}, nil
}

// Load returns the persisted thread ids, or nil when there is no usable file. A corrupt file is
// not an error worth failing a bot start over: the bot simply relearns its threads as it posts.
func (s *ThreadStore) Load() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil
	}
	return ids
}

// Save replaces the persisted set, writing through a temporary file so a crash mid-write cannot
// leave a truncated list behind. version orders concurrent writers: each snapshot carries the
// count it was taken at, and an older snapshot arriving late is dropped rather than allowed to
// erase a thread a newer one already recorded.
func (s *ThreadStore) Save(version uint64, ids []string) error {
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if version != 0 && version <= s.seen {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	if version > s.seen {
		s.seen = version
	}
	return nil
}
