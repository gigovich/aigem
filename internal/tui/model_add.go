package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/llm"
)

const (
	addProvider = iota
	addProviderID
	addBaseURL
	addAuth
	addModelID
	addName
	addContext
	addOutput
	addKey
	addReview
)

// modelAddForm shows one field at a time so text entry remains usable in short
// terminals. The review is scrollable, with its actions always visible.
type modelAddForm struct {
	providers                                           []llm.Provider
	provider                                            int // len(providers) selects a new compatible provider
	field                                               int
	providerID, baseURL, modelID, name, context, output string
	apiKey                                              bool
	key                                                 []rune
	checking, saving                                    bool
	status, destination, replacing                      string
	confirmed                                           bool
	selectAfter                                         bool
	scroll                                              int
	maxScroll                                           int
}

type modelReviewMsg struct {
	form                   *modelAddForm
	destination, replacing string
	err                    error
}

type modelSavedMsg struct {
	registry    *llm.Registry
	ref         string
	selectAfter bool
	err         error
}

type addedModelSelectedMsg struct {
	ref  string
	info llm.ModelInfo
	err  error
}

func (m *Model) openModelAdd() {
	if m.modelReg == nil {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "no model registry available"})
		m.refresh()
		return
	}
	m.models = nil
	m.modelAdd = &modelAddForm{providers: m.modelReg.AddableProviders(), apiKey: true}
	m.layout()
}

func (f *modelAddForm) providerInfo() llm.Provider {
	if f.provider < len(f.providers) {
		return f.providers[f.provider]
	}
	a := llm.AuthNone
	if f.apiKey {
		a = llm.AuthAPIKey
	}
	return llm.Provider{ID: strings.TrimSpace(f.providerID), BaseURL: strings.TrimSpace(f.baseURL), API: llm.APICompletions, Auth: a}
}

func (f *modelAddForm) acceptsKey() bool {
	switch f.providerInfo().Auth {
	case llm.AuthAPIKey, llm.AuthOpenAI, llm.AuthXAI:
		return true
	}
	return false
}

func (f *modelAddForm) fields() []int {
	fields := []int{addProvider}
	if f.provider == len(f.providers) {
		fields = append(fields, addProviderID, addBaseURL, addAuth)
	}
	fields = append(fields, addModelID, addName, addContext, addOutput)
	if f.acceptsKey() {
		fields = append(fields, addKey)
	}
	return append(fields, addReview)
}

func (f *modelAddForm) clearKey() {
	clear(f.key)
	f.key = nil
}

func (f *modelAddForm) textField() *string {
	switch f.field {
	case addProviderID:
		return &f.providerID
	case addBaseURL:
		return &f.baseURL
	case addModelID:
		return &f.modelID
	case addName:
		return &f.name
	case addContext:
		return &f.context
	case addOutput:
		return &f.output
	}
	return nil
}

func (f *modelAddForm) typeText(text string) {
	if f.checking || f.saving || f.field == addReview {
		return
	}
	text = flattenPaste(text)
	if f.field == addKey {
		f.key = append(f.key, []rune(text)...)
	} else if dst := f.textField(); dst != nil {
		*dst += text
	}
	f.status = ""
}

func trimLastRune(s string) string {
	_, n := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-n]
}

func (f *modelAddForm) addition() (llm.ModelAddition, error) {
	p := f.providerInfo()
	add := llm.ModelAddition{ProviderID: p.ID, Model: llm.ModelInfo{
		Provider: p.ID, ID: strings.TrimSpace(f.modelID), Name: strings.TrimSpace(f.name),
	}}
	if f.provider == len(f.providers) {
		add.Provider = &p
		if p.ID == "" || p.BaseURL == "" {
			return add, fmt.Errorf("provider ID and base URL are required")
		}
	}
	if add.Model.ID == "" {
		return add, fmt.Errorf("model ID is required")
	}
	for _, limit := range []struct {
		text, label string
		dst         *int
	}{
		{f.context, "context window", &add.Model.ContextWindow},
		{f.output, "output limit", &add.Model.MaxTokens},
	} {
		if s := strings.TrimSpace(limit.text); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				return add, fmt.Errorf("%s must be a positive integer or blank", limit.label)
			}
			*limit.dst = n
		}
	}
	return add, nil
}

func (m *Model) advanceModelAdd(delta int) tea.Cmd {
	f := m.modelAdd
	fields := f.fields()
	for i, field := range fields {
		if field != f.field {
			continue
		}
		next := fields[max(0, min(len(fields)-1, i+delta))]
		if next == addReview {
			add, err := f.addition()
			if err != nil {
				f.status = err.Error()
				m.layout()
				return nil
			}
			f.field, f.checking, f.confirmed, f.scroll = addReview, true, false, 0
			f.status = "Preparing review…"
			provider, hasKey := add.ProviderID, len(f.key) > 0
			m.layout()
			return func() tea.Msg {
				path, err := config.UserModelsFile()
				result := modelReviewMsg{form: f, destination: path, err: err}
				if err == nil && hasKey {
					rec, ok, err := auth.Get(provider)
					if err != nil {
						result.err = fmt.Errorf("cannot read credential store; repair it before saving a key")
					} else if ok {
						if rec.Kind == "oauth" {
							result.replacing = "OAuth credential"
						} else {
							result.replacing = "stored credential"
						}
					}
				}
				return result
			}
		}
		f.field, f.status, f.scroll = next, "", 0
		break
	}
	m.layout()
	return nil
}

func (m *Model) handleModelAddKey(msg tea.KeyPressMsg) tea.Cmd {
	f := m.modelAdd
	if f.saving {
		return nil // persistence has begun; Esc can no longer undo the write
	}
	if bareCode(msg) == tea.KeyEsc {
		f.clearKey()
		m.modelAdd = nil
		m.openModelPicker()
		return nil
	}
	if f.checking {
		return nil
	}
	if msg.String() == "shift+tab" {
		return m.advanceModelAdd(-1)
	}
	code := bareCode(msg)
	if f.field == addReview {
		switch code {
		case tea.KeyLeft, tea.KeyRight, tea.KeyTab:
			f.selectAfter = !f.selectAfter
		case tea.KeyPgUp, tea.KeyUp:
			f.scroll = max(0, f.scroll-1)
		case tea.KeyPgDown, tea.KeyDown:
			f.scroll = min(f.maxScroll, f.scroll+1)
		case ' ':
			if f.replacing != "" {
				f.confirmed = !f.confirmed
			}
		case tea.KeyEnter:
			if f.destination == "" {
				return m.advanceModelAdd(0)
			}
			if f.replacing != "" && !f.confirmed {
				f.status = "Space confirms credential replacement; Enter then saves."
				break
			}
			add, err := f.addition()
			if err != nil {
				f.status = err.Error()
				break
			}
			key := string(f.key)
			f.clearKey()
			f.saving, f.status = true, "Saving locally… (no endpoint probe)"
			m.layout()
			return saveAddedModel(m.modelReg, add, key, f.selectAfter)
		}
		m.layout()
		return nil
	}
	switch code {
	case tea.KeyPgUp:
		f.scroll = max(0, f.scroll-1)
	case tea.KeyPgDown:
		f.scroll = min(f.maxScroll, f.scroll+1)
	case tea.KeyEnter, tea.KeyTab:
		return m.advanceModelAdd(1)
	case tea.KeyUp, tea.KeyDown, tea.KeyLeft, tea.KeyRight:
		delta := 1
		if code == tea.KeyUp || code == tea.KeyLeft {
			delta = -1
		}
		if f.field == addProvider {
			f.provider = (f.provider + delta + len(f.providers) + 1) % (len(f.providers) + 1)
			f.clearKey()
			f.scroll = 0
		} else if f.field == addAuth {
			f.apiKey = !f.apiKey
			f.clearKey()
		} else {
			return m.advanceModelAdd(delta)
		}
	case tea.KeyBackspace:
		if f.field == addKey && len(f.key) > 0 {
			f.key[len(f.key)-1] = 0
			f.key = f.key[:len(f.key)-1]
		} else if dst := f.textField(); dst != nil && *dst != "" {
			*dst = trimLastRune(*dst)
		}
	default:
		f.typeText(msg.Text)
	}
	m.layout()
	return nil
}

func saveAddedModel(reg *llm.Registry, add llm.ModelAddition, key string, selectAfter bool) tea.Cmd {
	return func() tea.Msg {
		defer func() { key = "" }()
		fresh, err := reg.AddModel(add)
		result := modelSavedMsg{registry: fresh, ref: add.Model.Ref(), selectAfter: selectAfter}
		if err != nil {
			// Neither a backend error nor a filesystem error may echo a pasted key.
			text := err.Error()
			if key != "" {
				text = strings.ReplaceAll(text, key, "[redacted]")
			}
			result.err = fmt.Errorf("%s", text)
			return result
		}
		if key != "" {
			if err := auth.Put(add.ProviderID, auth.Record{Kind: "apikey", Key: key}); err != nil {
				result.err = fmt.Errorf("could not store API key; use aigem auth login %s", add.ProviderID)
			}
		}
		return result
	}
}

func (m *Model) applyModelSaved(msg modelSavedMsg) tea.Cmd {
	if msg.registry == nil {
		if f := m.modelAdd; f != nil {
			f.saving, f.destination = false, ""
			f.status = "Not saved: " + msg.err.Error() + ". API key cleared; Shift+Tab to edit."
		}
		m.layout()
		return nil
	}
	m.modelReg = msg.registry
	m.local.SetModelRegistry(msg.registry)
	m.modelAdd = nil
	if msg.err != nil {
		m.blocks = append(m.blocks, block{kind: bkError, text: "Saved, not selected: " + msg.ref + "; " + msg.err.Error()})
	} else if msg.selectAfter {
		// Opening a backend and writing the default may touch disk. Keep both off
		// the update loop, just like registry and credential persistence.
		m.busy = true
		m.layout()
		local := m.local
		return func() tea.Msg {
			info, err := local.SwitchModel(msg.ref, true)
			return addedModelSelectedMsg{ref: msg.ref, info: info, err: err}
		}
	} else {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "Saved " + msg.ref + "; current model and default unchanged. Endpoint not verified."})
	}
	m.openSavedModelPicker(msg.ref)
	return nil
}

func (m *Model) openSavedModelPicker(ref string) {
	m.openModelPicker()
	if m.models != nil {
		for i, it := range m.models.items {
			if it.kind == modelItemModel && it.ref == ref {
				m.models.cursor = i
				break
			}
		}
	}
	m.layout()
}

func (m Model) modelAddView() string {
	f, w := m.modelAdd, m.overlayInnerWidth()
	rows := []string{padLine(overlayTitleStyle.Render("Add model"), w, cSurface0)}
	var body []string
	p := f.providerInfo()
	add := func(s string) { body = append(body, wrapText(s, w)...) }
	hint := "Enter/Tab next · Shift+Tab back · Esc cancel"
	switch f.field {
	case addProvider:
		label := p.ID
		if f.provider == len(f.providers) {
			label = "New OpenAI-compatible provider"
		}
		add("Provider: " + label)
		if f.provider < len(f.providers) {
			add("Endpoint (read-only): " + p.BaseURL)
			add("Auth (read-only): " + p.Auth)
		}
		hint = "↑/↓ provider · Enter next · Esc cancel"
	case addAuth:
		add("Authentication: " + p.Auth)
		add("←/→ choose none or apikey")
	case addReview:
		add("Ref: " + p.ID + "/" + strings.TrimSpace(f.modelID))
		if f.replacing != "" {
			mark := "[ ]"
			if f.confirmed {
				mark = "[x]"
			}
			add(mark + " Replace " + f.replacing + " for every model on " + p.ID + ". Space to confirm.")
		}
		add("User config: " + f.destination)
		add("Endpoint: " + p.BaseURL + " · Auth: " + p.Auth)
		add("Name: " + blankFallback(f.name, "model ID"))
		add("Context: " + blankFallback(f.context, "startup context window"))
		add("Output: " + blankFallback(f.output, "startup output limit"))
		add("Will save locally; endpoint and model availability are not verified.")
		if len(f.key) > 0 {
			add("API key: supplied (stored separately in auth.json)")
		} else {
			add("API key: unchanged; existing credentials/environment apply.")
		}
		add("Save keeps the current/default model. Save & select makes this the default after a successful switch.")
		hint = "←/→ choice · Enter save · ↑/↓ review · Shift+Tab edit · Esc cancel"
		if f.replacing != "" && !f.confirmed {
			hint = "Space confirm replacement · Enter save · ↑/↓ review · Esc cancel"
		}
	default:
		labels := [...]string{
			addProviderID: "Provider ID (required; no slash)",
			addBaseURL:    "Base URL (required; http:// or https://)",
			addModelID:    "Model ID (required; slashes allowed)",
			addName:       "Display name (blank uses model ID)",
			addContext:    "Context window in tokens (blank uses startup window)",
			addOutput:     "Output limit in tokens (blank uses startup limit)",
			addKey:        "API key (optional; blank keeps existing credentials/environment)",
		}
		add(labels[f.field])
		value := ""
		if f.field == addKey {
			value = strings.Repeat("*", len(f.key))
		} else if dst := f.textField(); dst != nil {
			value = *dst
		}
		// Show the tail, including the cursor, when an entry is wider than the
		// panel. In particular a pasted key never reaches rendered output.
		runes := []rune(value)
		if len(runes) > max(1, w-2) {
			runes = runes[len(runes)-max(1, w-2):]
		}
		add(string(runes) + "_")
	}
	if f.saving {
		hint = "Saving… please wait"
	}
	hints := wrapText(hint, w)
	// Reserve every hint row so a narrow review keeps the status bar visible.
	limit := max(1, m.height-m.input.Height()-8-len(hints))
	if f.status != "" {
		limit = max(1, limit-1)
	}
	start := min(f.scroll, max(0, len(body)-limit))
	if f.textField() != nil || f.field == addKey {
		start = max(0, len(body)-limit) // keep the edited value/cursor visible
	}
	f.maxScroll = max(0, len(body)-limit)
	for _, line := range body[start:min(len(body), start+limit)] {
		rows = append(rows, padLine(overlayTextStyle.Render(line), w, cSurface0))
	}
	if f.status != "" {
		rows = append(rows, padLine(overlayWarnStyle.Render(truncate(f.status, w)), w, cSurface0))
	}
	if f.field == addReview && !f.saving && !f.checking {
		a, b := optSelStyle, optStyle
		if f.selectAfter {
			a, b = b, a
		}
		rows = append(rows, padLine(a.Render("Save")+" "+b.Render("Save & select"), w, cSurface0))
	}
	for _, line := range hints {
		rows = append(rows, padLine(overlayHintStyle.Render(line), w, cSurface0))
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func blankFallback(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "blank → " + fallback
}
