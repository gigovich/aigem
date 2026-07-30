// Package search gives the agent a web-search capability. It persists the chosen
// provider plus its credentials, and exposes a web_search tool the agent can call
// to look up information newer than the model's training cutoff (package
// versions, current docs, recent releases).
//
// The config file may hold a secret (the Brave API key) so it lives in the
// private state dir with 0600 perms, alongside auth.json.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gigovich/aigem/internal/config"
)

// Provider identifiers persisted in the config file.
const (
	ProviderBrave   = "brave"
	ProviderBrowser = "browser"
)

const (
	BrowserModeInteractive  = "interactive"
	BrowserEngineGoogle     = "google"
	BrowserEngineDuckDuckGo = "duckduckgo"
	BrowserEngineBing       = "bing"
)

// Config is the persisted search setup: which provider, plus per-provider creds.
type Config struct {
	Provider string         `json:"provider"`
	Brave    *BraveConfig   `json:"brave,omitempty"`
	Browser  *BrowserConfig `json:"browser,omitempty"`
}

// BraveConfig holds the Brave Search subscription token.
type BraveConfig struct {
	APIKey string `json:"api_key,omitempty"`
}

// BrowserConfig controls the local browser backend. It drives Chrome/Chromium via
// DevTools, opens search/results in an isolated profile, and extracts rendered
// page text from the browser.
type BrowserConfig struct {
	Engine     string `json:"engine,omitempty"`      // google | duckduckgo | bing
	Mode       string `json:"mode,omitempty"`        // interactive
	Executable string `json:"executable,omitempty"`  // optional browser executable
	ProfileDir string `json:"profile_dir,omitempty"` // optional Chrome/Chromium user-data-dir

	// Interactive-tester settings, used only by the browser_action tool. TestHosts
	// lists hostnames the tester may reach past the public-only guard (the app
	// under test is usually on an internal host). Credentials for fill steps come
	// from the agent itself (read from a local file or ticket, typed directly).
	TestHosts []string `json:"test_hosts,omitempty"`
}

// Result is one web-search hit.
type Result struct {
	Title       string
	URL         string
	Description string
}

// Searcher runs a query and returns ranked results.
type Searcher interface {
	// Search returns up to count results for query. count<=0 means a default.
	Search(ctx context.Context, query string, count int) ([]Result, error)
}

var fileMu sync.Mutex // serializes read-modify-write of search.json

// configPath returns ~/.local/state/aigem/search.json.
func configPath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "search.json"), nil
}

// Load reads the search config, returning a zero Config (Provider=="") when no
// config file exists yet - the signal for first-run setup.
func Load() (Config, error) {
	fileMu.Lock()
	defer fileMu.Unlock()
	return loadLocked()
}

func loadLocked() (Config, error) {
	path, err := configPath()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	if len(data) == 0 {
		return Config{}, nil
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, nil
}

// DefaultBrowserProfileDir returns the private Chrome/Chromium profile directory
// aigem uses when the browser backend is configured without an explicit profile.
func DefaultBrowserProfileDir() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "browser-profile"), nil
}

// PrepareBrowserConfig normalizes browser settings, fills a private profile
// directory when none was provided, and creates that profile directory.
func PrepareBrowserConfig(cfg *BrowserConfig) (BrowserConfig, error) {
	b := normalizeBrowserConfig(cfg)
	if b.ProfileDir == "" {
		dir, err := DefaultBrowserProfileDir()
		if err != nil {
			return BrowserConfig{}, err
		}
		b.ProfileDir = dir
	}
	if err := validateBrowserConfig(&b); err != nil {
		return BrowserConfig{}, err
	}
	if err := os.MkdirAll(b.ProfileDir, 0o700); err != nil {
		return BrowserConfig{}, fmt.Errorf("create browser profile dir: %w", err)
	}
	return b, nil
}

// Save writes the config with 0600 perms (it carries the API key).
func Save(c Config) error {
	fileMu.Lock()
	defer fileMu.Unlock()
	path, err := configPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	// WriteFile does not tighten perms on a pre-existing looser file; enforce 0600
	// since this holds a secret.
	return os.Chmod(path, 0o600)
}

// Clear removes the search config (a missing file is not an error).
func Clear() error {
	fileMu.Lock()
	defer fileMu.Unlock()
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Exists reports whether a config file is present, i.e. setup has been run.
func Exists() bool {
	path, err := configPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Enabled reports whether the config selects a usable, fully-credentialed
// provider (so a web_search tool can be built from it).
func (c Config) Enabled() bool {
	switch c.Provider {
	case ProviderBrave:
		return c.Brave != nil && c.Brave.APIKey != ""
	case ProviderBrowser:
		return validateBrowserConfig(c.Browser) == nil
	default:
		return false
	}
}

// Searcher builds the configured provider's Searcher, or an error if the config
// is incomplete or names an unsupported provider.
func (c Config) Searcher() (Searcher, error) {
	switch c.Provider {
	case ProviderBrave:
		if c.Brave == nil || c.Brave.APIKey == "" {
			return nil, fmt.Errorf("brave search selected but no API key is set")
		}
		return newBrave(c.Brave.APIKey), nil
	case ProviderBrowser:
		if err := validateBrowserConfig(c.Browser); err != nil {
			return nil, err
		}
		return newBrowser(c.Browser), nil
	case "":
		return nil, fmt.Errorf("no search provider configured")
	default:
		return nil, fmt.Errorf("unknown search provider %q", c.Provider)
	}
}

// Describe returns a short human-readable status line for the config.
func (c Config) Describe() string {
	switch c.Provider {
	case ProviderBrave:
		if c.Brave != nil && c.Brave.APIKey != "" {
			return "brave (API key set)"
		}
		return "brave (no API key)"
	case ProviderBrowser:
		b := normalizeBrowserConfig(c.Browser)
		desc := "browser (" + b.Engine + ", " + b.Mode
		if b.Executable != "" {
			desc += ", executable set"
		}
		if b.ProfileDir != "" {
			desc += ", profile set"
		}
		return desc + ")"
	case "":
		return "not configured"
	default:
		return c.Provider
	}
}
