package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/gigovich/aigem/internal/llm"
)

func TestAddModelOpensTheProviderPicker(t *testing.T) {
	m := modelAdditionTestModel(t)
	m.openModelPicker()
	m.models.cursor = len(m.models.items) - 1
	if m.models.items[m.models.cursor].kind != modelItemAdd {
		t.Fatal("the picker does not end with Add model…")
	}
	m = step(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.models == nil || m.models.stage != pickProvider || !m.models.loading {
		t.Fatalf("Add model… did not open the provider picker: %+v", m.models)
	}
	var providers []string
	for _, it := range m.models.items {
		if it.kind == modelItemProvider {
			providers = append(providers, it.provider)
		}
	}
	if len(providers) != 2 || providers[0] != llm.OpenAIProviderID || providers[1] != llm.XAIProviderID {
		t.Fatalf("providers = %v", providers)
	}
}

func TestCandidatesAreCheckedWithSpaceAndSavedTogether(t *testing.T) {
	m := modelAdditionTestModel(t)
	m.openProviderPicker()
	const base = "http://127.0.0.1:1234"
	m = step(m, localServersMsg{servers: []llm.LocalServer{{Name: "lmstudio", BaseURL: base, Models: []llm.ModelInfo{
		{ID: "a", ContextWindow: 100}, {ID: "b", ContextWindow: 200}, {ID: "c", ContextWindow: 300},
	}}}})
	for i, it := range m.models.items {
		if it.kind == modelItemServer {
			m.models.cursor = i
		}
	}
	m = step(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.models == nil || m.models.stage != pickCandidates || len(m.models.items) != 3 {
		t.Fatalf("the server's models are not listed: %+v", m.models)
	}
	m = step(m, keyPress(' '))
	m = step(m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(m, keyPress(' '))
	m = step(m, keyPress(' '))
	m = step(m, keyPress(' '))
	if m.models.query != "" || !m.models.items[0].checked || !m.models.items[1].checked || m.models.items[2].checked {
		t.Fatalf("Space did not toggle: %+v", m.models.items)
	}
	cmd := m.handleModelKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not save")
	}
	msg := cmd().(modelSavedMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	m = step(m, msg)
	for ref, want := range map[string]bool{"lmstudio/a": true, "lmstudio/b": true, "lmstudio/c": false} {
		if _, _, err := m.modelReg.Resolve(ref); (err == nil) != want {
			t.Errorf("%s resolves = %v, want %v", ref, err == nil, want)
		}
	}
	if p, _ := m.modelReg.Provider("lmstudio"); p.BaseURL != base || p.NeedsAuth() {
		t.Fatalf("server provider = %+v", p)
	}
}

func TestNoModelAtStartupOffersToAddOne(t *testing.T) {
	m := modelAdditionTestModel(t)
	m.model = ""
	m = step(m, m.checkActiveAvailability()())
	if m.alert == nil || m.alert.action != alertAddModel {
		t.Fatalf("alert = %+v, want one offering to add a model", m.alert)
	}
	if cmd := m.handleAlertKey(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd == nil {
		t.Fatal("confirming did not start looking for local servers")
	}
	if m.alert != nil || m.models == nil || m.models.stage != pickProvider {
		t.Fatal("confirming did not open the provider picker")
	}
}

func TestUnknownContextIsAskedForThenSavedWithTheChecked(t *testing.T) {
	m := modelAdditionTestModel(t)
	const base = "http://127.0.0.1:1234"
	m.openProviderPicker()
	m.showCandidates("", base, []llm.ModelInfo{{ID: "known", ContextWindow: 100}, {ID: "unknown"}})
	m = step(m, keyPress(' '))
	m = step(m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(m, keyPress(' '))
	if cmd := m.handleModelKey(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil || !m.models.askCtx {
		t.Fatalf("Enter did not ask for the context window: %+v", m.models)
	}
	for _, r := range "4096" {
		m = step(m, keyPress(r))
	}
	if m.models.ctx != "4096" || m.models.query != "" {
		t.Fatalf("digits went to ctx=%q query=%q", m.models.ctx, m.models.query)
	}
	cmd := m.handleModelKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not save")
	}
	msg := cmd().(modelSavedMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	m = step(m, msg)
	for ref, want := range map[string]int{"lmstudio/known": 100, "lmstudio/unknown": 4096} {
		if _, mi, err := m.modelReg.Resolve(ref); err != nil || mi.ContextWindow != want {
			t.Errorf("%s = %+v, %v, want context %d", ref, mi, err, want)
		}
	}
}

func TestALockedProviderWithoutTUILoginPointsToTheCLI(t *testing.T) {
	m := modelAdditionTestModel(t)
	t.Setenv("XAI_API_KEY", "")
	m.openProviderPicker()
	isXAI := func(it modelItem) bool { return it.provider == llm.XAIProviderID }
	m.models.cursor = slices.IndexFunc(m.models.items, isXAI)
	if m.models.cursor < 0 || !m.models.items[m.models.cursor].locked {
		t.Fatal("xai is not offered locked")
	}
	m = step(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if last := m.blocks[len(m.blocks)-1]; !strings.Contains(last.text, "aigem auth login xai") {
		t.Fatalf("last block = %q, want the CLI login hint", last.text)
	}
}

func TestDeleteOnAModelTheUserDidNotAddShowsWhy(t *testing.T) {
	m := modelAdditionTestModel(t)
	m.openModelPicker()
	m.models.cursor = slices.IndexFunc(m.models.items, func(it modelItem) bool { return it.ref == "local/test" })
	if m.models.cursor < 0 {
		t.Fatal("the local model is not listed")
	}
	m = step(m, tea.KeyPressMsg{Code: tea.KeyDelete})
	if m.models == nil || !strings.Contains(m.models.status, "not one you added") {
		t.Fatalf("picker = %+v, want the registry's refusal", m.models)
	}
	if _, _, err := m.modelReg.Resolve("local/test"); err != nil {
		t.Fatal("the local model was removed")
	}
}
