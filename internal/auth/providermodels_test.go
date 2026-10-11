package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
)

// modelsRegistry isolates credentials and config and builds a registry from the
// given user and project models.json texts.
func modelsRegistry(t *testing.T, user, project string) *llm.Registry {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	t.Cleanup(ResetSources)
	cwd := t.TempDir()
	userFile, err := config.UserModelsFile()
	if err != nil {
		t.Fatal(err)
	}
	for path, text := range map[string]string{userFile: user, config.ProjectModelsFile(cwd): project} {
		if text == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg, warnings := llm.NewRegistry(cwd, llm.Provider{})
	if len(warnings) != 0 {
		t.Fatal(warnings)
	}
	return reg
}

func TestProjectOnlyProviderGetsNoStoredCredential(t *testing.T) {
	reg := modelsRegistry(t, "", `[{"id":"repo","base_url":"https://repo.test","api":"openai-completions",
		"auth":"apikey","models":[{"id":"m"}]}]`)
	if err := Put("repo", Record{Kind: KindAPIKey, Key: "secret"}); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := OpenModel(reg, "repo/m", 0)
	if err == nil || !strings.Contains(err.Error(), "declared only by this project") {
		t.Fatalf("OpenModel = %v, want it refused as project-only", err)
	}
	if usable := UsableModels(reg, 0); usable["repo/m"] {
		t.Fatal("a project-only provider is usable with a stored key")
	}
	if _, err := ProviderModels(context.Background(), reg, "repo"); err == nil {
		t.Fatal("ProviderModels listed a project-only provider")
	}
}

func TestProviderModels(t *testing.T) {
	ctx := context.Background()
	oauth := func(provider string) {
		t.Helper()
		tok := &oauth2.Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}
		if err := Put(provider, Record{Kind: KindOAuth, Token: tok}); err != nil {
			t.Fatal(err)
		}
	}

	reg := modelsRegistry(t, "", "")
	if _, err := ProviderModels(ctx, reg, "nope"); err == nil {
		t.Fatal("an unknown provider was listed")
	}
	if _, err := ProviderModels(ctx, reg, llm.XAIProviderID); err == nil || !strings.Contains(err.Error(), "sign in") {
		t.Fatalf("ProviderModels without a credential = %v, want sign in first", err)
	}

	oauth(llm.OpenAIProviderID)
	models, err := ProviderModels(ctx, reg, llm.OpenAIProviderID)
	if err != nil || len(models) == 0 {
		t.Fatalf("ChatGPT subscription models = %+v, %v", models, err)
	}
	for _, m := range models {
		if !llm.IsCodexSubscriptionModel(m.ID) || m.Provider != llm.OpenAIProviderID {
			t.Fatalf("listed %+v, not a subscription model", m)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	reg = modelsRegistry(t, `[{"id":"xai","base_url":"`+srv.URL+`"}]`, "")
	oauth(llm.XAIProviderID)
	models, err = ProviderModels(ctx, reg, llm.XAIProviderID)
	if err != nil || len(models) == 0 || len(models) != len(llm.CatalogModels(llm.XAIProviderID)) {
		t.Fatalf("refused Grok listing = %+v, %v, want the catalog", models, err)
	}
}
