package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/oauth2"

	"github.com/gigovich/aigem/internal/config"
)

// oauthState is the persisted OAuth material for one server: the discovered
// endpoints, the (dynamically registered or preconfigured) client, and the
// current token including its refresh token. Stored 0600 since it holds secrets.
type oauthState struct {
	Resource      string        `json:"resource"`
	RedirectURL   string        `json:"redirect_url"`
	AuthEndpoint  string        `json:"authorization_endpoint"`
	TokenEndpoint string        `json:"token_endpoint"`
	Scopes        []string      `json:"scopes,omitempty"`
	ClientID      string        `json:"client_id"`
	ClientSecret  string        `json:"client_secret,omitempty"`
	Token         *oauth2.Token `json:"token,omitempty"`
}

// oauthDir returns ~/.local/state/aigem/mcp-oauth, creating the base.
func oauthDir() (string, error) {
	base, err := config.StateDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "mcp-oauth")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// safeName maps a server name to a filesystem-safe token file name.
func safeName(server string) string {
	var b strings.Builder
	for _, r := range server {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func oauthStatePath(server string) (string, error) {
	dir, err := oauthDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, safeName(server)+".json"), nil
}

// loadOAuthState returns the stored state for server, or nil if none exists.
func loadOAuthState(server string) (*oauthState, error) {
	path, err := oauthStatePath(server)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st oauthState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func saveOAuthState(server string, st *oauthState) error {
	path, err := oauthStatePath(server)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// deleteOAuthState removes stored credentials for server (logout). A missing
// file is not an error.
func deleteOAuthState(server string) error {
	path, err := oauthStatePath(server)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
