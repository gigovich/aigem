package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/gigovich/aigem/internal/search"
)

// agentItem is one row in the /agents browser: a delegated subagent or the
// special configurable web-search capability.
type agentItem struct {
	name         string
	description  string
	prompt       string   // subagent system prompt (empty for web-search)
	tools        []string // tools the subagent may use
	configurable bool     // web-search: selecting it opens a config editor
}

// agentBrowser is the scrollable agent list shown by /agents, with a per-agent
// detail view and, for configurable agents, an inline config editor.
type agentBrowser struct {
	items      []agentItem
	cursor     int
	detail     bool
	editing    bool   // config editor open (web-search)
	provider   string // brave | browser
	field      int    // selected field in the config editor
	keyBuf     string // Brave API key being typed in the editor
	engine     string // browser engine
	profileBuf string // optional browser profile dir
	status     string // editor feedback (verifying/error)
	saving     bool   // a verify+save command is in flight
}

// agentCfgDoneMsg reports the result of the background web-search verify+save.
type agentCfgDoneMsg struct {
	cfg             search.Config
	clearedProvider string
	err             error
}

const (
	agentFieldProvider = iota
	agentFieldBraveKey
	agentFieldBrowserEngine
	agentFieldBrowserProfile
	agentFieldClearProvider
)

var browserEngines = []string{search.BrowserEngineDuckDuckGo, search.BrowserEngineGoogle, search.BrowserEngineBing}
var searchProviderChoices = []string{search.ProviderBrave, search.ProviderBrowser}

func (m *Model) openAgents() {
	if m.agents == nil {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "no agents found"})
		m.refresh()
		return
	}
	var items []agentItem
	for _, d := range m.agents.List() {
		items = append(items, agentItem{
			name: d.Name, description: d.Description, prompt: d.Prompt, tools: d.Tools,
		})
	}
	cfg, _ := search.Load()
	items = append(items, agentItem{
		name:         "web-search",
		description:  webSearchDesc(cfg),
		configurable: true,
	})
	m.agentBr = &agentBrowser{items: items}
	m.layout()
}

func webSearchDesc(c search.Config) string {
	return "Web search the agent uses to look up current information " +
		"(versions, latest docs, recent releases). Provider: " + c.Describe() + "."
}

func (m *Model) initAgentConfigEditor() {
	cfg, _ := search.Load()
	b := m.agentBr
	b.editing = true
	b.status, b.saving = "", false
	b.keyBuf, b.profileBuf = "", ""
	b.provider = cfg.Provider
	switch b.provider {
	case search.ProviderBrave, search.ProviderBrowser:
	default:
		b.provider = search.ProviderBrave
	}
	b.engine = search.BrowserEngineDuckDuckGo
	if cfg.Browser != nil {
		if cfg.Browser.Engine != "" {
			b.engine = cfg.Browser.Engine
		}
		b.profileBuf = cfg.Browser.ProfileDir
	}
	b.field = agentFieldProvider
}

func (m *Model) handleAgentKey(msg tea.KeyPressMsg) tea.Cmd {
	b := m.agentBr
	if b.editing {
		return m.handleAgentEditKey(msg)
	}
	switch bareCode(msg) {
	case tea.KeyUp:
		if !b.detail && b.cursor > 0 {
			b.cursor--
		}
		m.refresh()
	case tea.KeyDown:
		if !b.detail && b.cursor < len(b.items)-1 {
			b.cursor++
		}
		m.refresh()
	case tea.KeyEnter:
		it := b.items[b.cursor]
		if !b.detail {
			b.detail = true // first Enter opens detail
			m.refresh()
			return nil
		}
		// Enter in detail opens the config editor for a configurable agent;
		// non-configurable subagents have nothing to run from here.
		if it.configurable {
			m.initAgentConfigEditor()
			m.layout()
		}
	case tea.KeyEsc:
		if b.detail {
			b.detail = false // back to the list
			m.refresh()
			return nil
		}
		m.agentBr = nil
		m.layout()
	}
	return nil
}

// handleAgentEditKey drives the web-search config editor: it switches providers,
// collects provider-specific fields, and saves the config in the background.
func (m *Model) handleAgentEditKey(msg tea.KeyPressMsg) tea.Cmd {
	b := m.agentBr
	if b.saving {
		return nil // ignore input while validation/probe runs
	}
	switch bareCode(msg) {
	case tea.KeyEnter:
		if b.field == agentFieldClearProvider {
			b.saving = true
			b.status = "clearing " + b.provider + " config..."
			m.layout()
			return clearSearchProvider(b.provider)
		}
		cfg, err := b.searchConfig()
		if err != nil {
			b.status = "error: " + err.Error()
			m.layout()
			return nil
		}
		b.saving = true
		if b.provider == search.ProviderBrave {
			b.status = "verifying key..."
		} else {
			b.status = "saving browser config and opening profile..."
		}
		m.layout()
		return saveSearchCfg(cfg)
	case tea.KeyEsc:
		b.editing, b.keyBuf, b.profileBuf, b.status = false, "", "", ""
		m.layout()
	case tea.KeyTab:
		b.nextConfigField()
		m.refresh()
	case tea.KeyUp:
		b.prevConfigField()
		m.refresh()
	case tea.KeyDown:
		b.nextConfigField()
		m.refresh()
	case tea.KeyLeft:
		b.adjustConfigChoice(-1)
		m.refresh()
	case tea.KeyRight:
		b.adjustConfigChoice(1)
		m.refresh()
	case tea.KeyBackspace:
		if b.backspaceConfigField() {
			m.refresh()
		}
	default:
		if msg.Text != "" {
			b.typeConfigField(msg.Text)
			m.refresh()
		}
	}
	return nil
}

func (b *agentBrowser) visibleConfigFields() []int {
	fields := []int{agentFieldProvider}
	switch b.provider {
	case search.ProviderBrowser:
		return append(fields, agentFieldBrowserEngine, agentFieldBrowserProfile, agentFieldClearProvider)
	case search.ProviderBrave:
		return append(fields, agentFieldBraveKey, agentFieldClearProvider)
	default:
		return fields
	}
}

func (b *agentBrowser) nextConfigField() {
	fields := b.visibleConfigFields()
	for i, f := range fields {
		if f == b.field {
			b.field = fields[(i+1)%len(fields)]
			return
		}
	}
	b.field = fields[0]
}

func (b *agentBrowser) prevConfigField() {
	fields := b.visibleConfigFields()
	for i, f := range fields {
		if f == b.field {
			b.field = fields[(i+len(fields)-1)%len(fields)]
			return
		}
	}
	b.field = fields[0]
}

func (b *agentBrowser) adjustConfigChoice(delta int) {
	switch b.field {
	case agentFieldProvider:
		idx := 0
		for i, p := range searchProviderChoices {
			if p == b.provider {
				idx = i
				break
			}
		}
		idx = (idx + delta + len(searchProviderChoices)) % len(searchProviderChoices)
		b.provider = searchProviderChoices[idx]
		b.field = agentFieldProvider
	case agentFieldBrowserEngine:
		idx := 0
		for i, e := range browserEngines {
			if e == b.engine {
				idx = i
				break
			}
		}
		idx = (idx + delta + len(browserEngines)) % len(browserEngines)
		b.engine = browserEngines[idx]
	}
}

func (b *agentBrowser) typeConfigField(s string) {
	switch b.field {
	case agentFieldBraveKey:
		b.keyBuf += s
	case agentFieldBrowserProfile:
		b.profileBuf += s
	}
}

func (b *agentBrowser) backspaceConfigField() bool {
	trim := func(s string) (string, bool) {
		r := []rune(s)
		if len(r) == 0 {
			return s, false
		}
		return string(r[:len(r)-1]), true
	}
	var ok bool
	switch b.field {
	case agentFieldBraveKey:
		b.keyBuf, ok = trim(b.keyBuf)
	case agentFieldBrowserProfile:
		b.profileBuf, ok = trim(b.profileBuf)
	}
	return ok
}

func (b *agentBrowser) searchConfig() (search.Config, error) {
	switch b.provider {
	case search.ProviderBrave:
		key := strings.TrimSpace(b.keyBuf)
		if key == "" {
			return search.Config{}, errors.New("a Brave API key is required")
		}
		return search.Config{Provider: search.ProviderBrave, Brave: &search.BraveConfig{APIKey: key}}, nil
	case search.ProviderBrowser:
		browserCfg, err := search.PrepareBrowserConfig(&search.BrowserConfig{
			Engine:     b.engine,
			Mode:       search.BrowserModeInteractive,
			ProfileDir: strings.TrimSpace(b.profileBuf),
		})
		if err != nil {
			return search.Config{}, err
		}
		cfg := search.Config{Provider: search.ProviderBrowser, Browser: &browserCfg}
		if _, err := cfg.Searcher(); err != nil {
			return search.Config{}, err
		}
		return cfg, nil
	default:
		return search.Config{}, fmt.Errorf("unknown search provider %q", b.provider)
	}
}

// saveSearchCfg verifies Brave keys when needed, then persists the config. The
// hot-swap into the live tool registry happens on the UI goroutine when the
// agentCfgDoneMsg is handled.
func saveSearchCfg(cfg search.Config) tea.Cmd {
	return func() tea.Msg {
		if cfg.Provider == search.ProviderBrave {
			s, err := cfg.Searcher()
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, err = s.Search(ctx, "hello", 1)
				cancel()
			}
			if err != nil {
				return agentCfgDoneMsg{err: fmt.Errorf("key verification failed: %w", err)}
			}
		} else if _, err := cfg.Searcher(); err != nil {
			return agentCfgDoneMsg{err: err}
		}
		merged, err := mergeSearchConfig(cfg)
		if err != nil {
			return agentCfgDoneMsg{err: err}
		}
		if cfg.Provider == search.ProviderBrowser {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			err := search.WarmBrowserProfile(ctx, *cfg.Browser)
			cancel()
			if err != nil {
				return agentCfgDoneMsg{err: fmt.Errorf("open browser profile: %w", err)}
			}
		}
		if err := search.Save(merged); err != nil {
			return agentCfgDoneMsg{err: err}
		}
		return agentCfgDoneMsg{cfg: merged}
	}
}

func mergeSearchConfig(cfg search.Config) (search.Config, error) {
	merged, err := search.Load()
	if err != nil {
		return search.Config{}, err
	}
	merged.Provider = cfg.Provider
	switch cfg.Provider {
	case search.ProviderBrave:
		merged.Brave = cfg.Brave
	case search.ProviderBrowser:
		merged.Browser = cfg.Browser
	}
	return merged, nil
}

func clearSearchProvider(provider string) tea.Cmd {
	return func() tea.Msg {
		cfg, err := search.Load()
		if err != nil {
			return agentCfgDoneMsg{err: err}
		}
		switch provider {
		case search.ProviderBrave:
			cfg.Brave = nil
			if cfg.Provider == search.ProviderBrave {
				cfg.Provider = ""
				if cfg.Browser != nil && (search.Config{Provider: search.ProviderBrowser, Browser: cfg.Browser}).Enabled() {
					cfg.Provider = search.ProviderBrowser
				}
			}
		case search.ProviderBrowser:
			cfg.Browser = nil
			if cfg.Provider == search.ProviderBrowser {
				cfg.Provider = ""
				if cfg.Brave != nil && cfg.Brave.APIKey != "" {
					cfg.Provider = search.ProviderBrave
				}
			}
		default:
			return agentCfgDoneMsg{err: fmt.Errorf("unknown search provider %q", provider)}
		}
		if cfg.Provider == "" && cfg.Brave == nil && cfg.Browser == nil {
			if err := search.Clear(); err != nil {
				return agentCfgDoneMsg{err: err}
			}
			return agentCfgDoneMsg{cfg: cfg, clearedProvider: provider}
		}
		if err := search.Save(cfg); err != nil {
			return agentCfgDoneMsg{err: err}
		}
		return agentCfgDoneMsg{cfg: cfg, clearedProvider: provider}
	}
}

// applyAgentCfg handles the verify+save result: on success it hot-swaps the
// web_search tool into the running agent's registry and advertises it in the
// system prompt, so the capability is usable this session.
func (m *Model) applyAgentCfg(msg agentCfgDoneMsg) {
	b := m.agentBr
	if b == nil || !b.editing {
		return
	}
	b.saving = false
	if msg.err != nil {
		b.status = "error: " + msg.err.Error()
		m.layout()
		return
	}
	if t := search.NewTool(msg.cfg); t != nil {
		// Mutating the shared registry / system prompt here is race-free only
		// because the /agents overlay can open just when no turn is running (the
		// dispatch that opens it is gated on !busy, and the overlay swallows all
		// keys), so the agent goroutine is not reading them concurrently.
		m.reg.Register(t)
		if bt := search.NewBrowseTool(msg.cfg); bt != nil {
			m.reg.Register(bt)
		} else {
			m.reg.Unregister("open_url")
		}
		if at := search.NewBrowserActionTool(msg.cfg); at != nil {
			m.reg.Register(at)
		} else {
			m.reg.Unregister("browser_action")
		}
		if !m.searchPrompted {
			m.agent.AppendSystem(search.Prompt(msg.cfg))
			m.searchPrompted = true
		} else {
			m.agent.AppendSystem("# Web search update\n\nThe web_search tool is now configured as: " + msg.cfg.Describe() + ".")
		}
	} else {
		m.reg.Unregister("web_search")
		m.reg.Unregister("open_url")
		m.reg.Unregister("browser_action")
		m.agent.AppendSystem("# Web search update\n\nThe web_search tool is now disabled.")
		m.searchPrompted = false
	}
	for i := range b.items {
		if b.items[i].configurable {
			b.items[i].description = webSearchDesc(msg.cfg)
		}
	}
	b.editing, b.detail, b.keyBuf, b.status = false, false, "", ""
	notice := "web search configured (" + msg.cfg.Describe() + ") - active now"
	if msg.clearedProvider != "" {
		notice = msg.clearedProvider + " search settings cleared"
		if msg.cfg.Provider != "" {
			notice += " - active provider: " + msg.cfg.Describe()
		} else {
			notice += " - web search inactive now"
		}
	}
	m.blocks = append(m.blocks, block{kind: bkNotice, text: notice})
	m.layout()
}

// ---- views ----

func (m Model) agentBrowserView() string {
	w := m.overlayInnerWidth()
	b := m.agentBr
	if b.editing {
		return m.agentConfigView(w)
	}
	if b.detail {
		return m.agentDetailView(b.items[b.cursor], w)
	}
	const maxRows = 8
	start := 0
	if b.cursor >= maxRows {
		start = b.cursor - maxRows + 1
	}
	nameW := max(8, w/3)
	title := overlayTitleStyle.Render("Agents  ") +
		overlayHintStyle.Render(fmt.Sprintf("(%d · ↑/↓ · Enter view · Esc close)", len(b.items)))
	rows := []string{padLine(title, w, cSurface0)}
	for i := start; i < len(b.items) && i < start+maxRows; i++ {
		it := b.items[i]
		name := it.name
		if it.configurable {
			name += " ⚙"
		}
		line := fmt.Sprintf(" %-*s  %s", nameW, truncate(name, nameW),
			truncate(oneLine(it.description), max(8, w-nameW-4)))
		if i == b.cursor {
			rows = append(rows, pickSelStyle.Width(w).MaxWidth(w).Render(line))
		} else {
			rows = append(rows, pickRowStyle.Width(w).MaxWidth(w).Render(line))
		}
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m Model) agentDetailView(it agentItem, w int) string {
	var rows []string
	add := func(st lipgloss.Style, text string) {
		rows = append(rows, padLine(st.Render(text), w, cSurface0))
	}
	add(overlayTitleStyle, "◆ "+it.name)
	for _, l := range wrapText(it.description, w) {
		add(overlayTextStyle, l)
	}
	if len(it.tools) > 0 {
		add(overlayHintStyle, "tools: "+strings.Join(it.tools, ", "))
	}
	for _, l := range firstNLines(it.prompt, 6) {
		add(resultStyle.Foreground(cOverlay0).Background(cSurface0), "  "+truncate(l, w-2))
	}
	if it.configurable {
		add(overlayHintStyle, "Enter configure · Esc back")
	} else {
		add(overlayHintStyle, "Esc back")
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m Model) agentConfigView(w int) string {
	b := m.agentBr
	var rows []string
	add := func(st lipgloss.Style, text string) {
		rows = append(rows, padLine(st.Render(text), w, cSurface0))
	}
	add(overlayTitleStyle, "◆ web-search · configure")
	add(overlayTextStyle, b.configRow(agentFieldProvider, "Provider", b.provider))
	if b.provider == search.ProviderBrowser {
		add(overlayTextStyle, b.configRow(agentFieldBrowserEngine, "Engine", b.engine))
		add(overlayTextStyle, b.configRow(agentFieldBrowserProfile, "Profile dir", b.profileBuf))
		add(overlayTextStyle, b.configRow(agentFieldClearProvider, "Clear provider", "delete browser settings"))
		add(overlayHintStyle, "automated: opens search/results in an isolated browser profile; blank profile creates a new one")
	} else {
		field := strings.Repeat("•", len([]rune(b.keyBuf)))
		if !b.saving && b.field == agentFieldBraveKey {
			field += "▌"
		}
		add(overlayTextStyle, b.configRow(agentFieldBraveKey, "API key", field))
		add(overlayTextStyle, b.configRow(agentFieldClearProvider, "Clear provider", "delete Brave settings"))
	}
	if b.status != "" {
		add(overlayHintStyle, b.status)
	}
	if b.saving {
		add(overlayHintStyle, "saving...")
	} else {
		add(overlayHintStyle, "↑/↓ or Tab fields · ←/→ choices · type text · Enter save/clear · Esc cancel")
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (b *agentBrowser) configRow(field int, label, value string) string {
	if value == "" {
		value = "—"
	}
	cursor := " "
	if b.field == field {
		cursor = "›"
		if !b.saving && field == agentFieldBrowserProfile {
			value += "▌"
		}
	}
	return fmt.Sprintf("%s %-17s %s", cursor, label+":", value)
}
