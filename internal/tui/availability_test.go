package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gigovich/aigem/internal/llm"
)

// registryWith builds a registry from a models.json holding one extra provider,
// the way a user adding a self-hosted endpoint would.
func registryWith(t *testing.T, providerJSON string) *llm.Registry {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir := filepath.Join(cfg, "aigem")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"providers":[` + providerJSON + `]}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	reg, warns := llm.NewUserRegistry(llm.LocalProvider("http://127.0.0.1:9280", "m.gguf", 8192, 4096))
	if len(warns) > 0 {
		t.Fatalf("models.json warnings: %v", warns)
	}
	return reg
}

// A provider declared with "auth": "none" - a self-hosted OpenAI-compatible
// endpoint such as Ollama, vLLM, or a second llama.cpp - needs no credential.
// Reporting it as unauthenticated puts a modal alert in front of a model that
// works, and tells the user to run a login that would fail.
func TestSelfHostedProviderIsNotReportedUnauthenticated(t *testing.T) {
	reg := registryWith(t, `{
		"id":"selfhosted","base_url":"http://127.0.0.1:11434","api":"openai-completions",
		"auth":"none","models":[{"provider":"selfhosted","id":"m","context_window":8192}]}`)

	if a := assessActiveModel("selfhosted/m", reg); a != nil {
		t.Errorf("an auth-less provider was reported unavailable: %q / %q", a.title, a.body)
	}
}

// The real case must still be reported: a provider that requires a credential,
// with none stored, is genuinely unusable.
func TestUnauthenticatedProviderIsStillReported(t *testing.T) {
	reg := registryWith(t, `{
		"id":"needsauth","base_url":"https://api.example.com","api":"openai-completions",
		"auth":"apikey","models":[{"provider":"needsauth","id":"m","context_window":8192}]}`)

	if a := assessActiveModel("needsauth/m", reg); a == nil {
		t.Error("a provider needing auth with no stored credential was not reported")
	}
}
