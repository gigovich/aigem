package bot

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFleetConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path, err := FleetConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFleetConfigDefaultsWithNoFile(t *testing.T) {
	writeFleetConfig(t, "")
	c, err := LoadFleetConfig()
	if err != nil {
		t.Fatalf("a missing fleet.json means the defaults, not an error: %v", err)
	}
	if c.TurnCap() != DefaultMaxConcurrentTurns || c.BrowserCap() != DefaultMaxConcurrentBrowsers {
		t.Fatalf("caps = %d/%d, want the defaults", c.TurnCap(), c.BrowserCap())
	}
}

func TestFleetConfigOverridesAndUncaps(t *testing.T) {
	writeFleetConfig(t, `{"max_concurrent_turns": 12, "max_concurrent_browsers": -1}`)
	c, err := LoadFleetConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.TurnCap() != 12 {
		t.Fatalf("turn cap = %d, want 12", c.TurnCap())
	}
	// A negative value asks for no cap, which the limiters express as a nil limiter.
	if c.BrowserCap() != 0 {
		t.Fatalf("browser cap = %d, want 0 (uncapped)", c.BrowserCap())
	}
	if NewTurnLimiter(c.BrowserCap()) != nil {
		t.Fatal("an uncapped setting must produce no limiter")
	}
}

func TestFleetConfigRejectsAMalformedFile(t *testing.T) {
	// Silently running an unbounded fleet because a comma was misplaced is the failure this
	// config exists to prevent.
	writeFleetConfig(t, `{"max_concurrent_turns": 6,}`)
	if _, err := LoadFleetConfig(); err == nil {
		t.Fatal("a malformed fleet.json must be an error, not a silent fallback to the defaults")
	}
}
