package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/web"
)

func TestAddModelsFromAServerCreatesAProviderAndSwapsTheRegistry(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg, _ := llm.NewRegistry(t.TempDir(), llm.Provider{})
	b := newWebBackend(webBackendConfig{version: "test", models: sharedModels(reg)})
	ctx := context.Background()

	var refusal *web.Refusal
	_, err := b.AddModels(ctx, web.NewModels{BaseURL: "http://127.0.0.1:1234", Models: []web.Model{{Ref: "m"}}})
	if !errors.As(err, &refusal) {
		t.Fatalf("a model without a context window was added: %v", err)
	}

	added, err := b.AddModels(ctx, web.NewModels{BaseURL: "http://127.0.0.1:1234",
		Models: []web.Model{{Ref: "org/m", ContextWindow: 4096}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0].Ref != "lmstudio/org/m" {
		t.Fatalf("added = %+v", added)
	}
	p, m, err := b.models.Load().Resolve("lmstudio/org/m")
	if err != nil || p.BaseURL != "http://127.0.0.1:1234" || p.NeedsAuth() || m.ContextWindow != 4096 {
		t.Fatalf("registry not swapped: %+v %+v %v", p, m, err)
	}
	if err := b.RemoveModel(ctx, "lmstudio/org/m"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.models.Load().Resolve("lmstudio/org/m"); err == nil {
		t.Fatal("the removed model still resolves")
	}
}

func TestRemoveModelRefusesWhatTheUserDidNotAdd(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cwd := t.TempDir()
	project := config.ProjectModelsFile(cwd)
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	projectJSON := `{"providers":[{"id":"repo","base_url":"https://repo.test","models":[{"id":"m"}]}]}`
	if err := os.WriteFile(project, []byte(projectJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, _ := llm.NewRegistry(cwd, llm.LocalProvider("http://127.0.0.1:9280", "gemma.gguf", 1000, 100))
	b := newWebBackend(webBackendConfig{version: "test", models: sharedModels(reg)})
	for _, ref := range []string{"local/gemma.gguf", "repo/m", "openai/gpt-5.6-sol", "nope"} {
		var refusal *web.Refusal
		if err := b.RemoveModel(context.Background(), ref); !errors.As(err, &refusal) {
			t.Fatalf("%s: RemoveModel = %v, want a refusal", ref, err)
		}
	}
	if b.models.Load() != reg {
		t.Fatal("a refused removal swapped the registry")
	}
}

func TestServerURLDropsTheV1Suffix(t *testing.T) {
	for typed, want := range map[string]string{
		"http://127.0.0.1:8000":      "http://127.0.0.1:8000",
		"http://127.0.0.1:8000/":     "http://127.0.0.1:8000",
		"http://127.0.0.1:8000/v1":   "http://127.0.0.1:8000",
		"http://127.0.0.1:8000/v1/":  "http://127.0.0.1:8000",
		"https://host.test/proxy/v1": "https://host.test/proxy",
	} {
		if got, err := serverURL(typed); err != nil || got != want {
			t.Errorf("serverURL(%q) = %q, %v, want %q", typed, got, err, want)
		}
	}
	if _, err := serverURL("ftp://host"); err == nil {
		t.Error("a non-http URL was accepted")
	}
}

func TestPreferredModelIgnoresAProjectOnlyPreference(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cwd := t.TempDir()
	project := config.ProjectModelsFile(cwd)
	if err := os.MkdirAll(filepath.Dir(project), 0o700); err != nil {
		t.Fatal(err)
	}
	projectJSON := `{"providers":[{"id":"repo","base_url":"https://repo.test","models":[{"id":"m"}]}]}`
	if err := os.WriteFile(project, []byte(projectJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveModelPref("repo/m"); err != nil {
		t.Fatal(err)
	}
	reg, _ := llm.NewRegistry(cwd, llm.Provider{})
	if got := preferredModelRef(reg); got != "" {
		t.Fatalf("preferredModelRef = %q, want none", got)
	}
	reg, err := reg.AddModel(llm.ModelAddition{ProviderID: "local-test",
		Provider: &llm.Provider{
			ID:      "local-test",
			BaseURL: "http://127.0.0.1:9",
			API:     llm.APICompletions,
			Auth:    llm.AuthNone,
		},
		Models: []llm.ModelInfo{{ID: "a", ContextWindow: 100}, {ID: "b", ContextWindow: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveModelPref("local-test/b"); err != nil {
		t.Fatal(err)
	}
	if got := preferredModelRef(reg); got != "local-test/b" {
		t.Fatalf("preferredModelRef = %q, want the saved added model", got)
	}
}

func TestConcurrentAddModelsKeepBoth(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	reg, _ := llm.NewRegistry(t.TempDir(), llm.Provider{})
	b := newWebBackend(webBackendConfig{version: "test", models: sharedModels(reg)})
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Go(func() {
			_, err := b.AddModels(context.Background(), web.NewModels{Provider: llm.OpenAIProviderID,
				Models: []web.Model{{Ref: "openai/" + id, ContextWindow: 1000}}})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for _, ref := range []string{"openai/a", "openai/b"} {
		if _, _, err := b.models.Load().Resolve(ref); err != nil {
			t.Errorf("%s lost: %v", ref, err)
		}
	}
}
