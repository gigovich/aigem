package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Prefs holds user choices that persist across restarts, stored in
// ~/.local/state/aigem/preferences.json.
type Prefs struct {
	Model string `json:"model,omitempty"` // last selected model ref (provider/id)
}

var prefsMu sync.Mutex // serializes read-modify-write of preferences.json

func prefsPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "preferences.json"), nil
}

// LoadPrefs reads saved preferences, returning a zero Prefs when none exist or
// the file is unreadable - preferences are best-effort and never block startup.
func LoadPrefs() Prefs {
	prefsMu.Lock()
	defer prefsMu.Unlock()
	path, err := prefsPath()
	if err != nil {
		return Prefs{}
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return Prefs{}
	}
	var p Prefs
	if err := json.Unmarshal(data, &p); err != nil {
		return Prefs{}
	}
	return p
}

// SaveModelPref persists ref as the last selected model, preserving any other
// preferences already on disk.
func SaveModelPref(ref string) error {
	prefsMu.Lock()
	defer prefsMu.Unlock()
	path, err := prefsPath()
	if err != nil {
		return err
	}
	p := Prefs{}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &p)
	}
	p.Model = ref
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
