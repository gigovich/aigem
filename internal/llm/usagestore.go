package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/gigovich/aigem/internal/config"
)

// Quota state is observed as a side effect of real calls, so a snapshot is kept
// on disk: `aigem usage` can then report where the account stands without
// spending a request of its own, and a bot's observation is visible to the CLI.
//
// One file per provider, not one file with every provider in it: five bot
// processes and the CLI all write these, and a shared file would mean a
// read-modify-write race in which one process's rename drops another provider's
// entry outright. Per-provider files make the rename the whole update.
const limitsDir = "usage"

func limitsDirPath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, limitsDir), nil
}

// providerFile maps a provider id to its snapshot path. Ids come from a config
// file, so anything path-like in one is replaced rather than trusted.
func providerFile(dir, provider string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == os.PathSeparator || r == '.' {
			return '_'
		}
		return r
	}, provider)
	return filepath.Join(dir, safe+".json")
}

// SaveLimits records l as the newest snapshot for its provider. A zero Limits is
// ignored: a provider that reports no quota headers must not erase what an
// earlier call learned.
func SaveLimits(l Limits) error {
	if l.IsZero() || l.Provider == "" {
		return nil
	}
	dir, err := limitsDirPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := providerFile(dir, l.Provider)
	// Concurrent writers for one provider still race, but they are all writing
	// that provider's own state, so the loser is simply an older reading - which
	// this drops rather than publishing.
	if prev, ok := readLimitsFile(path); ok && !l.ObservedAt.After(prev.ObservedAt) {
		return nil
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".snapshot.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadLimits returns the stored snapshot per provider. Missing or unreadable
// snapshots are skipped: a usage report is informational and never fatal.
func LoadLimits() map[string]Limits {
	out := map[string]Limits{}
	dir, err := limitsDirPath()
	if err != nil {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		l, ok := readLimitsFile(filepath.Join(dir, e.Name()))
		if !ok || l.Provider == "" {
			continue
		}
		out[l.Provider] = l
	}
	return out
}

func readLimitsFile(path string) (Limits, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return Limits{}, false
	}
	var l Limits
	if err := json.Unmarshal(data, &l); err != nil {
		return Limits{}, false
	}
	return l, true
}
