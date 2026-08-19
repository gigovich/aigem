package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
)

func testModelAdmin(t *testing.T) *fleetModelAdmin {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	// Clearing validates the developer role default before persisting. A dummy
	// key is sufficient because opening the client performs no network request.
	t.Setenv("OPENAI_API_KEY", "test-only")
	models := filepath.Join(root, "aigem", "models.json")
	if err := os.MkdirAll(filepath.Dir(models), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(models, []byte(`{"providers":[{"id":"test","base_url":"http://127.0.0.1:1","api":"openai-completions","auth":"none","models":[{"id":"small","name":"Test Small"}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bot.Save(bot.Config{Name: "amiran", Role: "developer", Workdir: "."}); err != nil {
		t.Fatal(err)
	}
	return newFleetModelAdmin([]string{"amiran"}, newLiveFleet([]string{"amiran"}))
}

func TestFleetModelAdminNormalizesPersistsAndClears(t *testing.T) {
	admin := testModelAdmin(t)
	bare := "small"
	got, err := admin.SetModel(t.Context(), "amiran", &bare)
	if err != nil {
		t.Fatal(err)
	}
	if got.Configured != "test/small" || got.Selected != "test/small" || got.Source != bot.ModelSourceConfigured {
		t.Fatalf("normalized settings = %+v", got)
	}
	saved, err := bot.Load("amiran")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Model != "test/small" {
		t.Fatalf("saved model = %q", saved.Model)
	}

	got, err = admin.SetModel(t.Context(), "amiran", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Configured != "" || got.Selected != bot.DefaultBotModel || got.Source != bot.ModelSourceRoleDefault {
		t.Fatalf("cleared settings = %+v", got)
	}
}

func TestFleetModelAdminRejectsUnknownBotAndUnusableModel(t *testing.T) {
	admin := testModelAdmin(t)
	ref := "test/small"
	if _, err := admin.SetModel(t.Context(), "../amiran", &ref); !errors.Is(err, chat.ErrNoSuchBot) {
		t.Fatalf("path-like unknown bot: %v, want ErrNoSuchBot", err)
	}
	bad := "openai/not-real"
	if _, err := admin.SetModel(t.Context(), "amiran", &bad); !errors.Is(err, chat.ErrInvalidModel) {
		t.Fatalf("unknown model: %v, want ErrInvalidModel", err)
	}
}

func TestFleetModelAdminProjectsStoppedAndRestartRequiredTruthfully(t *testing.T) {
	admin := testModelAdmin(t)
	ref := "test/small"
	if _, err := admin.SetModel(t.Context(), "amiran", &ref); err != nil {
		t.Fatal(err)
	}
	stopped, err := admin.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped.Bots) != 1 || stopped.Bots[0].Running != "" || stopped.Bots[0].RestartRequired {
		t.Fatalf("stopped projection = %+v", stopped.Bots)
	}

	running, err := admin.settings("amiran", map[string]chat.LiveBot{
		chat.BotActor("amiran"): {Running: true, Model: bot.DefaultBotModel},
	})
	if err != nil {
		t.Fatal(err)
	}
	if running.Running != bot.DefaultBotModel || !running.RestartRequired {
		t.Fatalf("mismatched running projection = %+v", running)
	}
	running, err = admin.settings("amiran", map[string]chat.LiveBot{
		chat.BotActor("amiran"): {Running: true, Model: "test/small"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if running.RestartRequired {
		t.Fatalf("matching projection still requires restart: %+v", running)
	}
}

func TestFleetModelAdminReturnsTrustedOptionsWithoutSecrets(t *testing.T) {
	admin := testModelAdmin(t)
	models, err := admin.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, option := range models.Options {
		if option.Ref == "test/small" {
			found = option.Usable && option.Name == "Test Small"
		}
		if strings.Contains(strings.ToLower(option.Reason), "token") && strings.Contains(option.Reason, "Bearer") {
			t.Fatalf("option reason appears to contain a credential: %q", option.Reason)
		}
	}
	if !found {
		t.Fatalf("trusted usable option missing: %+v", models.Options)
	}
}
