// Package daemonstate records where a running daemon can be reached.
//
// It exists because there is more than one: `aigem web run` serves the
// conversations you drive, `aigem bot start` serves the ones the fleet has on
// its own, and either may be running without the other. Each writes its own
// file; the shape and the rules are the same for both.
package daemonstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/gigovich/aigem/internal/config"
)

// State is how a client finds a running daemon.
type State struct {
	PID   int    `json:"pid"`
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

func path(name string) (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Save records a running daemon. 0600: the file holds the token that
// authorises everything the daemon can do, which is the same class of secret as
// the credential store.
func Save(name string, s State) error {
	p, err := path(name)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// Load returns the recorded daemon, and whether it is still running. A record
// whose process is gone is stale rather than an error: a daemon killed without
// a chance to clean up should not stop the next one from starting.
func Load(name string) (State, bool, error) {
	p, err := path(name)
	if err != nil {
		return State{}, false, err
	}
	b, err := os.ReadFile(p)
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

// Clear removes the record on a clean exit.
func Clear(name string) error {
	p, err := path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
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
