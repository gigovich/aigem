package uisession

import (
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/llm"
)

// A session built without a model registry cannot resolve a reference. It has
// to say so: dereferencing the nil registry would panic on the caller's
// goroutine, and in a process holding several conversations that takes all of
// them down over one client's request.
func TestSwitchModelWithoutARegistryIsAnError(t *testing.T) {
	l := newSession(t)

	_, err := l.SwitchModel("openai/gpt-5.6-sol", false)
	if err == nil {
		t.Fatal("switching model without a registry was accepted")
	}
	if !strings.Contains(err.Error(), "registry") {
		t.Fatalf("the error does not say what is missing: %v", err)
	}
}

func TestSetModelRegistryMakesSavedModelSwitchable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	local := llm.LocalProvider("http://127.0.0.1:9280", "old.gguf", 1000, 100)
	registry, _ := llm.NewRegistry(t.TempDir(), local)
	backend, err := llm.Open(local, local.Models[0], llm.Credential{Kind: llm.AuthNone}, 100)
	if err != nil {
		t.Fatal(err)
	}
	ref := llm.NewRef(backend)
	l := New(Config{Models: registry, Backend: ref, CtxSize: 1000})
	t.Cleanup(l.Close)
	fresh, err := registry.AddModel(llm.ModelAddition{
		ProviderID: "new-provider",
		Provider: &llm.Provider{
			ID: "new-provider", BaseURL: "https://example.com/v1",
			API: llm.APICompletions, Auth: llm.AuthNone,
		},
		Model: llm.ModelInfo{ID: "org/new", ContextWindow: 32000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.SwitchModel("new-provider/org/new", false); err == nil {
		t.Fatal("old registry unexpectedly resolved saved model")
	}
	l.SetModelRegistry(fresh)
	if ref.Model().Ref() != "local/old.gguf" || l.CtxSize() != 1000 {
		t.Fatal("installing registry changed active model")
	}
	info, err := l.SwitchModel("new-provider/org/new", false)
	if err != nil || info.Ref() != "new-provider/org/new" || ref.Model().Ref() != info.Ref() || l.CtxSize() != 32000 {
		t.Fatalf("saved model not switched: %+v %v", info, err)
	}
	withKey, err := fresh.AddModel(llm.ModelAddition{
		ProviderID: "needs-key",
		Provider: &llm.Provider{
			ID: "needs-key", BaseURL: "https://example.com/v1",
			API: llm.APICompletions, Auth: llm.AuthAPIKey,
		},
		Model: llm.ModelInfo{ID: "locked", ContextWindow: 64000},
	})
	if err != nil {
		t.Fatal(err)
	}
	l.SetModelRegistry(withKey)
	if _, err := l.SwitchModel("needs-key/locked", false); err == nil {
		t.Fatal("unauthenticated model switch accepted")
	}
	if ref.Model().Ref() != info.Ref() || l.CtxSize() != 32000 {
		t.Fatal("failed switch replaced active backend")
	}
}
