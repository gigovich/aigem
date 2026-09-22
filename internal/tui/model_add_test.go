package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
)

func modelAdditionTestModel(t *testing.T) Model {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := newTestModel(t)
	registry, warnings := llm.NewRegistry(t.TempDir(), llm.LocalProvider("http://127.0.0.1:9280", "test", 8192, 8192))
	if len(warnings) != 0 {
		t.Fatal(warnings)
	}
	m.modelReg = registry
	m.local.SetModelRegistry(registry)
	return step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
}

func customAddition(authMode string) llm.ModelAddition {
	return llm.ModelAddition{
		ProviderID: "addition-test",
		Provider:   &llm.Provider{ID: "addition-test", BaseURL: "http://127.0.0.1:9/v1", API: llm.APICompletions, Auth: authMode},
		Model:      llm.ModelInfo{Provider: "addition-test", ID: "org/model", ContextWindow: 16384, MaxTokens: 2048},
	}
}

func TestModelAddActionSurvivesNoMatches(t *testing.T) {
	m := modelAdditionTestModel(t)
	m.openModelPicker()
	m.models.cursor = len(m.models.items) - 1
	m.models.query = "local/test"
	m.models.filter()
	if it := m.models.items[m.models.cursor]; it.kind != modelItemModel || it.ref != "local/test" {
		t.Fatal("filtering a selection near the end unexpectedly selected Add instead of the matching model")
	}
	m.models.query = ""
	m.models.filter()
	m = step(m, tea.PasteMsg{Content: "no-such-model-界"})
	if len(m.models.items) != 1 || m.models.items[0].kind != modelItemAdd || m.models.items[0].ref != "" {
		t.Fatalf("expected a typed action, not a model reference: %+v", m.models.items)
	}
	if view := ansi.Strip(m.modelPickerView()); !strings.Contains(view, "Add model…") || !strings.Contains(view, "no matches") {
		t.Fatalf("action disappeared with no matches: %s", view)
	}
	m = step(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.modelAdd == nil || m.models != nil || m.input.Focused() {
		t.Fatal("Add action did not open a modal form")
	}
	for _, p := range m.modelAdd.providers {
		if p.ID == llm.LocalProviderID {
			t.Fatal("managed local setup offered for registration")
		}
	}
}

func TestModelAddCancelClearsSecretAndIgnoresLateReview(t *testing.T) {
	m := modelAdditionTestModel(t)
	m = typeEnter(m, "/model add")
	if m.modelAdd == nil {
		t.Fatal("/model add did not open the form")
	}
	f := m.modelAdd
	f.field, f.modelID = addKey, "org/new-model"
	secret := "secret-界-key"
	m = step(m, tea.PasteMsg{Content: secret})
	if strings.Contains(m.render(), secret) || m.input.Value() != "" {
		t.Fatal("key escaped the masked field")
	}
	m = step(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if string(f.key) != "secret-界-ke" || !utf8.ValidString(string(f.key)) {
		t.Fatalf("Unicode key editing corrupted input: %q", string(f.key))
	}
	f.field, f.name = addName, "名界"
	m = step(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if f.name != "名" {
		t.Fatalf("Unicode backspace = %q", f.name)
	}
	f.field = addKey
	cmd := m.handleModelAddKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("review should inspect credentials asynchronously")
	}
	secretBuffer := f.key
	m = step(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	m = step(m, cmd()) // cancelled review must not resurrect the secret/form
	if m.modelAdd != nil || len(f.key) != 0 {
		t.Fatal("cancel retained the form/key")
	}
	for _, r := range secretBuffer {
		if r != 0 {
			t.Fatal("cancel did not clear secret buffer")
		}
	}
	for _, entry := range m.history {
		if strings.Contains(entry, "secret-") {
			t.Fatal("secret entered history")
		}
	}
	path, err := config.UserModelsFile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancel wrote models: %v", err)
	}
	if _, ok, err := auth.Get(f.providerInfo().ID); err != nil || ok {
		t.Fatalf("cancel wrote credential: present=%v err=%v", ok, err)
	}
}

func TestModelAddReviewRequiresCredentialReplacementConfirmation(t *testing.T) {
	m := modelAdditionTestModel(t)
	if err := auth.Put("openai", auth.Record{Kind: "oauth"}); err != nil {
		t.Fatal(err)
	}
	m.openModelAdd()
	f := m.modelAdd
	for i, p := range f.providers {
		if p.ID == "openai" {
			f.provider = i
		}
	}
	f.field, f.modelID, f.key = addKey, "org/custom", []rune("replacement-secret")
	cmd := m.handleModelAddKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = step(m, cmd())
	if view := ansi.Strip(m.modelAddView()); !strings.Contains(view, "Replace OAuth credential") || strings.Contains(view, "replacement-secret") {
		t.Fatalf("review must disclose replacement without exposing the secret: %s", view)
	}
	if cmd := m.handleModelAddKey(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil || f.saving {
		t.Fatal("unconfirmed replacement started persistence")
	}
	m.handleModelAddKey(tea.KeyPressMsg{Code: ' ', Text: " "})
	cmd = m.handleModelAddKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || !f.saving || len(f.key) != 0 {
		t.Fatal("confirmed save did not start/clear form secret")
	}
	// The command is deliberately not run: pressing Save only schedules disk I/O.
	path, _ := config.UserModelsFile()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("update loop wrote config: %v", err)
	}
}

func TestModelAddSaveAndSelectLifecycle(t *testing.T) {
	for _, selectAfter := range []bool{false, true} {
		name := "save"
		if selectAfter {
			name = "save-and-select"
		}
		t.Run(name, func(t *testing.T) {
			m := modelAdditionTestModel(t)
			oldReg, oldRef, oldDefault := m.modelReg, m.backend.Model().Ref(), config.LoadPrefs().Model
			addition := customAddition(llm.AuthNone)
			msg := saveAddedModel(m.modelReg, addition, "", selectAfter)().(modelSavedMsg)
			if msg.err != nil || msg.registry == nil {
				t.Fatalf("save failed: %+v", msg)
			}
			cmd := m.applyModelSaved(msg)
			if _, _, err := oldReg.Resolve(addition.Model.Ref()); err == nil {
				t.Fatal("old registry mutated")
			}
			if _, _, err := m.modelReg.Resolve(addition.Model.Ref()); err != nil {
				t.Fatal(err)
			}
			if m.backend.Model().Ref() != oldRef || config.LoadPrefs().Model != oldDefault {
				t.Fatal("registration changed selection before the selection command ran")
			}
			if selectAfter {
				if cmd == nil {
					t.Fatal("selection was not scheduled")
				}
				m = step(m, cmd())
				if m.backend.Model().Ref() != addition.Model.Ref() || config.LoadPrefs().Model != addition.Model.Ref() {
					t.Fatal("save-and-select failed to install registry/select/default")
				}
				if m.local.CtxSize() != 16384 {
					t.Fatalf("context window = %d", m.local.CtxSize())
				}
			} else {
				if cmd != nil || m.models == nil || m.models.items[m.models.cursor].ref != addition.Model.Ref() {
					t.Fatal("Save should reopen picker highlighting the addition without switching")
				}
				m = step(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				if m.backend.Model().Ref() != addition.Model.Ref() {
					t.Fatal("picker could not select saved entry using refreshed session registry")
				}
			}
		})
	}
}

func TestModelAddSelectionFailureKeepsOriginalBackend(t *testing.T) {
	m := modelAdditionTestModel(t)
	oldRef, oldDefault := m.backend.Model().Ref(), config.LoadPrefs().Model
	addition := customAddition(llm.AuthAPIKey)
	msg := saveAddedModel(m.modelReg, addition, "", true)().(modelSavedMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	cmd := m.applyModelSaved(msg)
	m = step(m, cmd())
	if m.backend.Model().Ref() != oldRef || config.LoadPrefs().Model != oldDefault {
		t.Fatal("auth failure changed current/default model")
	}
	if !hasBlock(m, bkError, "Saved, not selected") || !hasBlock(m, bkError, "aigem auth login") {
		t.Fatal("selection failure was not distinguished from a failed save with login guidance")
	}
	if m.models == nil || !m.models.items[m.models.cursor].locked {
		t.Fatal("saved model missing or not locked")
	}
}

func TestModelAddCredentialFailureStillInstallsSavedRegistry(t *testing.T) {
	m := modelAdditionTestModel(t)
	oldRef := m.backend.Model().Ref()
	badState := filepath.Join(t.TempDir(), "file-not-directory")
	if err := os.WriteFile(badState, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", badState)
	addition := customAddition(llm.AuthAPIKey)
	msg := saveAddedModel(m.modelReg, addition, "private-key", true)().(modelSavedMsg)
	if msg.err == nil || msg.registry == nil {
		t.Fatalf("expected registration followed by credential failure: %+v", msg)
	}
	if cmd := m.applyModelSaved(msg); cmd != nil {
		t.Fatal("credential failure still tried to select")
	}
	if m.backend.Model().Ref() != oldRef {
		t.Fatal("credential failure changed backend")
	}
	if _, _, err := m.modelReg.Resolve(addition.Model.Ref()); err != nil {
		t.Fatal("persisted model was not installed", err)
	}
	if !hasBlock(m, bkError, "Saved, not selected") || strings.Contains(m.render(), "private-key") {
		t.Fatal("credential failure was misleading or exposed key")
	}
	path, _ := config.UserModelsFile()
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "private-key") {
		t.Fatalf("model persistence leaked credential: %v", err)
	}
}

func TestModelAddFailedRegistrationDoesNotWriteCredential(t *testing.T) {
	m := modelAdditionTestModel(t)
	addition := llm.ModelAddition{ProviderID: "openai", Model: llm.ModelInfo{Provider: "openai", ID: "gpt-5.6-sol"}}
	msg := saveAddedModel(m.modelReg, addition, "private-key", true)().(modelSavedMsg)
	if msg.err == nil || msg.registry != nil {
		t.Fatalf("duplicate should fail without a new registry: %+v", msg)
	}
	if _, ok, err := auth.Get("openai"); ok || err != nil {
		t.Fatalf("failed save wrote credential: present=%v err=%v", ok, err)
	}
}
