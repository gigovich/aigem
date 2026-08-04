package bot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gigovich/aigem/internal/config"
)

// FleetConfig holds the limits that apply to every bot in the process, stored in
// ~/.config/aigem/fleet.json. They are per-process, not per-bot, which is why they do not live in
// bot.yaml: a cap on how many turns run at once has no meaning for one bot in isolation.
//
// The defaults are what a five-bot team on one provider account wants; an operator with a larger
// quota, a faster account, or a bigger machine raises them without rebuilding.
type FleetConfig struct {
	// MaxConcurrentTurns is how many agent turns may run at once across every bot, counting
	// scheduled runs. Zero means the default; a negative value means no cap.
	MaxConcurrentTurns *int `json:"max_concurrent_turns,omitempty"`
	// MaxConcurrentBrowsers is how many browsers may run at once across every bot. Chrome is
	// started per tool call and closed after, so this bounds a peak, not a resident cost. Zero
	// means the default; a negative value means no cap.
	MaxConcurrentBrowsers *int `json:"max_concurrent_browsers,omitempty"`
}

// Fleet limit defaults. Six concurrent turns is what keeps a five-bot team under one provider
// account's rate limit; two browsers bounds the memory peak when several bots search at once.
const (
	DefaultMaxConcurrentTurns    = 6
	DefaultMaxConcurrentBrowsers = 2
)

// FleetConfigPath is where the fleet limits are read from.
func FleetConfigPath() (string, error) {
	dir, err := config.BotsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "fleet.json"), nil
}

// LoadFleetConfig reads the fleet limits. A missing file is not an error - it means the defaults.
// A malformed one is: silently running an unbounded fleet because a comma was misplaced is the
// failure this config exists to prevent.
func LoadFleetConfig() (FleetConfig, error) {
	path, err := FleetConfigPath()
	if err != nil {
		return FleetConfig{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return FleetConfig{}, nil
	}
	if err != nil {
		return FleetConfig{}, err
	}
	var c FleetConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return FleetConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// TurnCap is the configured concurrent-turn ceiling, or the default. A negative configured value
// means no cap, which NewTurnLimiter expresses as a nil limiter.
func (c FleetConfig) TurnCap() int {
	return resolveCap(c.MaxConcurrentTurns, DefaultMaxConcurrentTurns)
}

// BrowserCap is the configured concurrent-browser ceiling, or the default.
func (c FleetConfig) BrowserCap() int {
	return resolveCap(c.MaxConcurrentBrowsers, DefaultMaxConcurrentBrowsers)
}

// resolveCap maps an unset or zero value to the default and a negative one to "no cap", which the
// limiters read as any value below one.
func resolveCap(v *int, def int) int {
	if v == nil || *v == 0 {
		return def
	}
	if *v < 0 {
		return 0
	}
	return *v
}
