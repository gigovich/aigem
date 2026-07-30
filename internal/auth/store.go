// Package auth stores provider credentials (API keys, ChatGPT OAuth tokens,
// xAI Grok subscription OAuth tokens) and resolves them into llm.Credential
// values for the registry. Tokens are treated like passwords: the store is
// 0600 and never logged.
package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/oauth2"

	"github.com/gigovich/aigem/internal/config"
)

// Record is the stored credential for one provider. Kind selects the transport:
// "apikey" for a plain API key, "oauth" for a subscription OAuth token (the
// ChatGPT/Codex flow, or the xAI/Grok device-code flow).
type Record struct {
	Kind      string        `json:"kind"`            // apikey | oauth
	Key       string        `json:"key,omitempty"`   // API key (kind=apikey)
	Token     *oauth2.Token `json:"token,omitempty"` // OAuth token (kind=oauth)
	AccountID string        `json:"account_id,omitempty"`
	// TokenURL is the token endpoint refreshes must use, captured from OIDC
	// discovery at login for providers whose endpoint is not compiled in
	// (xai). Empty for the ChatGPT flow, which has a fixed endpoint.
	TokenURL string `json:"token_url,omitempty"`
}

// store is the on-disk auth.json: provider id -> Record.
type store map[string]Record

var fileMu sync.Mutex // serializes read-modify-write of auth.json

// authPath returns ~/.local/state/aigem/auth.json.
func authPath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "auth.json"), nil
}

// loadStore reads auth.json, returning an empty store if it does not exist.
func loadStore() (store, error) {
	path, err := authPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store{}, nil
		}
		return nil, err
	}
	s := store{}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return s, nil
}

// saveStore writes auth.json atomically with 0600 perms: it writes a temp file
// in the same directory and renames it over the target, so a crash or a
// concurrent writer (several bots share this file) can never leave a truncated
// auth.json that would lose every stored credential.
func saveStore(s store) error {
	path, err := authPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth-*.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Get returns the stored record for a provider, or ok=false if none.
func Get(provider string) (Record, bool, error) {
	fileMu.Lock()
	defer fileMu.Unlock()
	s, err := loadStore()
	if err != nil {
		return Record{}, false, err
	}
	r, ok := s[provider]
	return r, ok, nil
}

// Put stores (or replaces) the record for a provider.
func Put(provider string, r Record) error {
	fileMu.Lock()
	defer fileMu.Unlock()
	s, err := loadStore()
	if err != nil {
		return err
	}
	s[provider] = r
	return saveStore(s)
}

// Delete removes a provider's stored credential (logout). A missing record is
// not an error.
func Delete(provider string) error {
	fileMu.Lock()
	defer fileMu.Unlock()
	s, err := loadStore()
	if err != nil {
		return err
	}
	if _, ok := s[provider]; !ok {
		return nil
	}
	delete(s, provider)
	return saveStore(s)
}
