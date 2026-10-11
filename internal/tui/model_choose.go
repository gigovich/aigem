package tui

import (
	"context"
	"slices"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/llm"
)

// The stages of the /model picker: the added models, then where to add one
// from, then what that provider serves.
const (
	pickModels = iota
	pickProvider
	pickCandidates
)

type localServersMsg struct{ servers []llm.LocalServer }

type candidatesMsg struct {
	provider string
	models   []llm.ModelInfo
	err      error
}

// openProviderPicker lists where a model can be added from. The local servers
// arrive a moment later, once the probe answers.
func (m *Model) openProviderPicker() tea.Cmd {
	if m.modelReg == nil {
		return nil
	}
	mp := &modelPicker{stage: pickProvider, loading: true}
	for _, p := range m.modelReg.AddableProviders() {
		mp.all = append(mp.all, modelItem{
			kind: modelItemProvider, ref: p.ID, provider: p.ID, locked: p.NeedsAuth() && !auth.IsAuthenticated(p.ID),
		})
	}
	mp.all = append(mp.all,
		modelItem{kind: modelItemSetupLocal, ref: "Set up a local model (llama.cpp)"},
		modelItem{kind: modelItemManual, ref: "Enter by hand…"},
	)
	mp.filter()
	m.models = mp
	m.layout()
	reg := m.modelReg
	return func() tea.Msg { return localServersMsg{reg.DiscoverServers(context.Background())} }
}

func (m *Model) applyLocalServers(msg localServersMsg) {
	mp := m.models
	if mp == nil || mp.stage != pickProvider {
		return
	}
	mp.loading = false
	at := slices.IndexFunc(mp.all, func(it modelItem) bool { return it.kind == modelItemSetupLocal })
	var servers []modelItem
	for _, s := range msg.servers {
		servers = append(servers, modelItem{kind: modelItemServer, ref: s.BaseURL, name: s.Name, server: s.Models})
	}
	mp.all = slices.Insert(mp.all, at, servers...)
	mp.filter()
	m.layout()
}

func (m *Model) listProviderModels(provider string) tea.Cmd {
	m.models = &modelPicker{stage: pickCandidates, provider: provider, loading: true}
	m.layout()
	reg := m.modelReg
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		models, err := auth.ProviderModels(ctx, reg, provider)
		return candidatesMsg{provider: provider, models: models, err: err}
	}
}

func (m *Model) applyCandidates(msg candidatesMsg) {
	mp := m.models
	if mp == nil || mp.stage != pickCandidates || mp.provider != msg.provider {
		return
	}
	if msg.err != nil {
		mp.loading, mp.status = false, msg.err.Error()
		m.layout()
		return
	}
	m.showCandidates(msg.provider, "", msg.models)
}

// showCandidates lists what a provider, or the server at baseURL, serves that
// is not added yet.
func (m *Model) showCandidates(provider, baseURL string, models []llm.ModelInfo) {
	mp := &modelPicker{stage: pickCandidates, provider: provider, baseURL: baseURL}
	pid := provider
	if baseURL != "" {
		pid = m.modelReg.ServerAddition(baseURL).ProviderID
	}
	for _, mi := range m.modelReg.NotAdded(pid, models) {
		mp.all = append(mp.all, modelItem{kind: modelItemCandidate, ref: mi.ID, name: mi.Name, info: mi})
	}
	if len(mp.all) == 0 {
		mp.status = "Nothing new to add here."
	}
	mp.filter()
	m.models = mp
	m.layout()
}

func (mp *modelPicker) toggle() {
	if mp.cursor >= len(mp.items) {
		return
	}
	ref := mp.items[mp.cursor].ref
	for i := range mp.all {
		if mp.all[i].ref == ref {
			mp.all[i].checked = !mp.all[i].checked
		}
	}
	mp.filter()
}

// addCandidates saves the checked models, or the focused one when none is
// checked. Models whose context window nobody reported take the one typed.
func (m *Model) addCandidates() tea.Cmd {
	mp := m.models
	var chosen []llm.ModelInfo
	for _, it := range mp.all {
		if it.checked {
			chosen = append(chosen, it.info)
		}
	}
	if len(chosen) == 0 && mp.cursor < len(mp.items) {
		chosen = append(chosen, mp.items[mp.cursor].info)
	}
	if len(chosen) == 0 {
		return nil
	}
	add := llm.ModelAddition{ProviderID: mp.provider}
	if mp.baseURL != "" {
		add = m.modelReg.ServerAddition(mp.baseURL)
	}
	ctx, _ := strconv.Atoi(mp.ctx)
	for i := range chosen {
		chosen[i].Provider = add.ProviderID
		if chosen[i].ContextWindow > 0 {
			continue
		}
		if ctx <= 0 {
			mp.askCtx = true
			mp.status = "Type the context window for the models that do not report one, then Enter."
			m.layout()
			return nil
		}
		chosen[i].ContextWindow = ctx
	}
	add.Models = chosen
	mp.loading = true
	m.layout()
	return saveAddedModel(m.modelReg, add, "", m.backend.Model().ID == "")
}

// removeModel removes a model the user added. A conversation on it keeps the
// backend it has.
func (m *Model) removeModel(it modelItem) {
	fresh, err := m.modelReg.RemoveModel(it.ref)
	if err != nil {
		m.models.status = err.Error()
		m.layout()
		return
	}
	m.modelReg = fresh
	m.local.SetModelRegistry(fresh)
	m.openModelPicker()
	m.models.status = "Removed " + it.ref
}
