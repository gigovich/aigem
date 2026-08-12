package chat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/gigovich/aigem/internal/config"
)

// State is how `aigem chat` finds the running fleet, in the same shape and the
// same place as the session daemon's own record. It is a separate file because
// the two daemons are separate: `aigem web run` serves conversations you drive,
// `aigem bot start` serves the ones the fleet has on its own, and either may be
// running without the other.
type State struct {
	PID   int    `json:"pid"`
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

func statePath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "chat.json"), nil
}

// SaveState records a running fleet daemon. 0600: it holds the token that
// authorises everything the daemon can do.
func SaveState(s State) error {
	path, err := statePath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// LoadState returns the recorded daemon, and whether it is still running. A
// record whose process is gone is stale rather than an error: a daemon killed
// without a chance to clean up should not stop the next one from starting.
func LoadState() (State, bool, error) {
	path, err := statePath()
	if err != nil {
		return State{}, false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, false, nil
		}
		return State{}, false, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, false, nil // an unreadable record is a stale one
	}
	return s, alive(s.PID), nil
}

// ClearState removes the record on a clean exit.
func ClearState() error {
	path, err := statePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// alive reports whether a pid names a live process. Signal 0 checks for
// existence without delivering anything.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(signalZero) == nil
}
