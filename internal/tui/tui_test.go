package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/local"
	"github.com/gigovich/aigem/internal/search"
	"github.com/gigovich/aigem/internal/session"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

func typeEnter(m Model, s string) Model {
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return step(m, tea.KeyMsg{Type: tea.KeyEnter})
}

// trustedSkillProject returns a temp project holding .skills/<name>/SKILL.md, with HOME and
// the XDG dirs isolated from the real ones. The project must be trusted: Discover skips
// project-local skills entirely until then, so an untrusted project yields no skills.
func trustedSkillProject(t *testing.T, name, content string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestSkillsBrowserAndDispatch(t *testing.T) {
	cwd := trustedSkillProject(t, "greet",
		"---\nname: greet\ndescription: say hi nicely\n---\nSay hello to $ARGUMENTS.\n")
	skills, _ := skill.Discover(cwd)
	reg, _ := tools.NewRegistry(cwd)
	// Dead port: a started turn fails fast instead of hitting a real server.
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// /skills opens the browser; Enter shows detail.
	m = typeEnter(m, "/skills")
	if m.browser == nil || len(m.browser.items) != 1 {
		t.Fatalf("expected browser with 1 skill, got %+v", m.browser)
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.browser.detail || !strings.Contains(m.View(), "say hi nicely") {
		t.Fatalf("expected detail view with description")
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEsc}) // back to list
	m = step(m, tea.KeyMsg{Type: tea.KeyEsc}) // close
	if m.browser != nil {
		t.Fatal("browser should close")
	}

	// Unknown skill -> a notice, no turn (checked while idle).
	m = typeEnter(m, "/skill:nope")
	if !hasBlock(m, bkNotice, "no such skill: nope") {
		t.Fatal("expected unknown-skill notice")
	}

	// /skill:greet runs it: a distinct skill line is shown and the turn starts.
	// Drain events until the turn finishes so no goroutine outlives the test.
	m = typeEnter(m, "/skill:greet world")
	if !hasBlock(m, bkSkill, "greet world") || !m.busy {
		t.Fatalf("expected a skill line and a started turn, blocks: %+v", m.blocks)
	}
	for {
		msg := <-m.events
		m = step(m, msg)
		if _, ok := msg.(turnDoneMsg); ok {
			break
		}
	}
	if m.busy {
		t.Fatal("turn should end")
	}
}

func TestCommandMenu(t *testing.T) {
	cwd := trustedSkillProject(t, "greet",
		"---\nname: greet\ndescription: say hi\n---\nhi $ARGUMENTS\n")
	skills, _ := skill.Discover(cwd)
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// A bare "/" lists every command (/new, /model, /login, /logout, /resume,
	// /skills, /agents, /artifacts, /compact, /skill:greet).
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if m.cmdMenu == nil || len(m.cmdMenu.items) != 10 {
		t.Fatalf("expected menu with 10 commands, got %+v", m.cmdMenu)
	}

	// Typing fuzzy-filters: "sk" keeps /skills and /skill:greet, drops /resume.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("sk")})
	if m.cmdMenu == nil || len(m.cmdMenu.items) != 2 {
		t.Fatalf("expected 2 matches for /sk, got %+v", m.cmdMenu)
	}
	if !strings.Contains(m.View(), "Commands") {
		t.Fatal("expected command menu in view")
	}

	// Down then Enter runs the second match (/skill:greet).
	m = step(m, tea.KeyMsg{Type: tea.KeyDown})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.cmdMenu != nil {
		t.Fatal("menu should close after Enter")
	}
	if !hasBlock(m, bkSkill, "greet") || !m.busy {
		t.Fatalf("expected /skill:greet to run, blocks: %+v", m.blocks)
	}
	for {
		msg := <-m.events
		m = step(m, msg)
		if _, ok := msg.(turnDoneMsg); ok {
			break
		}
	}

	// Tab completes the input to the highlighted command without running it.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/res")})
	m = step(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.input.Value() != "/resume" {
		t.Fatalf("expected Tab to complete to /resume, got %q", m.input.Value())
	}

	// A space ends the command token and closes the menu.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	if m.cmdMenu != nil {
		t.Fatal("menu should close once a space is typed")
	}
}

func newTestModel(t *testing.T) Model {
	t.Helper()
	reg, err := tools.NewRegistry(".")
	if err != nil {
		t.Fatal(err)
	}
	skills, _ := skill.Discover(t.TempDir())
	return New(llm.NewRef(llm.New("http://127.0.0.1:9280", "test")), reg, 0.3, "test", "http://127.0.0.1:9280", "", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
}

// step applies one message and returns the updated concrete model.
func step(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestLocalChoiceWidgetLayout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // never touch the real local-model config
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	full := m.vp.Height

	// Picking a local model in /model opens the widget through the real key path.
	// It must relayout so the overlay fits on screen instead of pushing the
	// conversation off the top.
	item := modelItem{ref: "local/demo.gguf", provider: llm.LocalProviderID}
	m.models = &modelPicker{all: []modelItem{item}, items: []modelItem{item}}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.localChoice == nil {
		t.Fatal("selecting a local model should open the action widget")
	}
	if m.vp.Height >= full {
		t.Fatalf("viewport should shrink for the overlay: full=%d open=%d", full, m.vp.Height)
	}
	if h := lipgloss.Height(m.View()); h != m.height {
		t.Fatalf("widget View overflows screen: height=%d want=%d", h, m.height)
	}

	// Height stays constant as focus moves across the three actions.
	for _, idx := range []int{0, 1, 2} {
		m.localChoice.idx = idx
		if h := lipgloss.Height(m.View()); h != m.height {
			t.Fatalf("widget View overflows at idx %d: height=%d want=%d", idx, h, m.height)
		}
	}

	// Tab cycles focus and clamps at the last action; Esc closes and restores layout.
	m.localChoice.idx = 0
	m = step(m, tea.KeyMsg{Type: tea.KeyTab})
	m = step(m, tea.KeyMsg{Type: tea.KeyTab})
	m = step(m, tea.KeyMsg{Type: tea.KeyTab})
	if m.localChoice.idx != 2 {
		t.Fatalf("Tab should clamp at Drop (idx 2), got %d", m.localChoice.idx)
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.localChoice != nil {
		t.Fatal("Esc should close the widget")
	}
	if m.vp.Height != full {
		t.Fatalf("closing should restore viewport: full=%d now=%d", full, m.vp.Height)
	}

	// Confirming Drop must not delete immediately - it opens a confirmation alert.
	m.models = &modelPicker{all: []modelItem{item}, items: []modelItem{item}}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.localChoice.idx = 2
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.localChoice != nil {
		t.Fatal("confirming Drop should close the widget")
	}
	if m.alert == nil || m.alert.action != alertLocalDrop {
		t.Fatalf("Drop should open a confirmation alert, got %+v", m.alert)
	}
	if h := lipgloss.Height(m.View()); h != m.height {
		t.Fatalf("drop-confirm alert overflows screen: height=%d want=%d", h, m.height)
	}
}

// TestLocalStartPinsDownloadTo100 covers the bug where the progress bar never
// reached 100% when the model loaded from cache: completion must show a full bar
// even if no live PhaseReady frame arrived.
func TestLocalStartPinsDownloadTo100(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cwd := t.TempDir()
	reg, _ := tools.NewRegistry(cwd)
	skills, _ := skill.Discover(cwd)
	localProv := llm.LocalProvider("http://127.0.0.1:9280", "gemma.gguf", 262144, 8192)
	modelReg, _ := llm.NewRegistry(cwd, localProv)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9280", "gemma.gguf")), reg, 0.3,
		"local/gemma.gguf", "u", "", 262144, 8192, agent.DefaultSubagents(), "", skills, nil, nil, "",
		agent.CompactConfig{CtxSize: 262144}, modelReg)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Simulate a download in flight: a live block stuck mid-progress (never 100%).
	m.dlName = "gemma.gguf"
	m.busy = true
	m.blocks = append(m.blocks, block{kind: bkDownload,
		text: m.downloadBlockText(local.Progress{Phase: local.PhaseDownloading, Downloaded: 1, Total: 100})})
	m.localProgIdx = len(m.blocks) - 1
	idx := m.localProgIdx

	cfg := local.Config{ModelName: "gemma.gguf", Host: "127.0.0.1", Port: 9280, CtxSize: 262144}
	m = step(m, localStartedMsg{cfg: cfg})

	if m.localProgIdx != -1 {
		t.Fatalf("localProgIdx should reset after start, got %d", m.localProgIdx)
	}
	if !strings.Contains(m.blocks[idx].text, "100%") {
		t.Fatalf("completed download block should show 100%%, got %q", m.blocks[idx].text)
	}
}

func TestInputAutoExpandsAndCapsAtFourLines(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 24, Height: 20})
	if m.input.Height() != 1 {
		t.Fatalf("expected initial input height 1, got %d", m.input.Height())
	}
	initialVPHeight := m.vp.Height

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strings.Repeat("a", 80))})
	if m.input.Height() != maxInputHeight {
		t.Fatalf("expected input height %d, got %d", maxInputHeight, m.input.Height())
	}
	if m.vp.Height != initialVPHeight-(maxInputHeight-1) {
		t.Fatalf("expected viewport to shrink with input, was %d now %d", initialVPHeight, m.vp.Height)
	}

	m.input.Reset()
	if !m.resizeInputHeight() || m.input.Height() != 1 {
		t.Fatalf("expected reset input to shrink to height 1, got %d", m.input.Height())
	}
}

// typeRunes feeds s one rune at a time and renders after each keystroke, exactly
// as the live program does. Both matter: a bulk insert skips the transient
// per-keystroke sizing, and rendering each frame is what makes the textarea's
// stale scroll observable.
func typeRunes(m Model, s string) Model {
	for _, r := range s {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		_ = m.View()
	}
	return m
}

func firstInputRow(m Model) string {
	return strings.SplitN(m.input.View(), "\n", 2)[0]
}

func TestInputHeightMatchesWrappedRows(t *testing.T) {
	// A line whose content exactly fills the width gains a trailing phantom row
	// in the textarea; char-wrap math under-counts it by one.
	cases := []struct {
		value string
		width int
		want  int
	}{
		{"", 44, 1},
		{"hello", 44, 1},
		{strings.Repeat("a", 44), 44, 2},
		{strings.Repeat("a", 80), 44, 2},
		{"the quick brown fox jumps over the lazy dog and then keeps running", 44, 2},
	}
	for _, c := range cases {
		if got := inputHeight(c.value, c.width); got != c.want {
			t.Errorf("inputHeight(%q, %d) = %d, want %d", c.value, c.width, got, c.want)
		}
	}
}

func TestWrappingInputKeepsFirstLineVisible(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 50, Height: 20})
	// Typing char-by-char drives the textarea past the exactly-fills-width
	// boundary, which previously left its viewport scrolled with line one hidden.
	m = typeRunes(m, "the quick brown fox jumps over the lazy dog and then keeps running")
	view := m.input.View()
	if !strings.Contains(firstInputRow(m), "the quick brown fox") {
		t.Fatalf("first wrapped line scrolled out of view:\n%s", view)
	}
	if !strings.Contains(view, "and then keeps running") {
		t.Fatalf("last wrapped line missing:\n%s", view)
	}
}

func TestInputShrinksBackToTopAfterOverflow(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 50, Height: 20})
	// Overflow past the four-line cap so the textarea scrolls, then delete back
	// down. Once everything fits again the first line must be visible, not left
	// scrolled under the top border.
	m = typeRunes(m, "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike "+
		"november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu extra")
	if m.input.Height() != maxInputHeight {
		t.Fatalf("expected overflow to cap at %d, got %d", maxInputHeight, m.input.Height())
	}
	// Backspace all the way down. At every step where the text fits in the box,
	// the first word must stay visible: a stale downward scroll left from the
	// overflow would clip the first line under the top border.
	fitSeen := false
	for i := 0; i < 300 && len(m.input.Value()) > 5; i++ {
		m = step(m, tea.KeyMsg{Type: tea.KeyBackspace})
		_ = m.View()
		if inputRows(m.input.Value(), m.input.Width()) > maxInputHeight {
			continue // still overflowing - the viewport correctly follows the cursor
		}
		fitSeen = true
		// The value still starts with "alpha" here; it must head the first row.
		if !strings.Contains(firstInputRow(m), "alpha") {
			t.Fatalf("step %d height=%d: first line clipped after shrink:\n%s", i, m.input.Height(), m.input.View())
		}
	}
	if !fitSeen {
		t.Fatal("test never reached a fitting state")
	}
}

func TestImagePasteInsertsInputMarker(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("look")})

	m = step(m, imagePasteMsg{image: llm.Image{MediaType: "image/png", Data: "AAA="}})
	if got := m.input.Value(); got != "look [image:1] " {
		t.Fatalf("input marker = %q", got)
	}
	if len(m.pendingImages) != 1 {
		t.Fatalf("pendingImages = %d, want 1", len(m.pendingImages))
	}

	m = step(m, tea.KeyMsg{Type: tea.KeyEsc})
	if got := m.input.Value(); got != "look" {
		t.Fatalf("Esc should remove image markers, got %q", got)
	}
	if len(m.pendingImages) != 0 {
		t.Fatalf("pendingImages after Esc = %d, want 0", len(m.pendingImages))
	}
}

func TestImageMarkerHelpers(t *testing.T) {
	if got := imageMarker(2); got != "[image:2]" {
		t.Fatalf("imageMarker = %q", got)
	}
	if got := stripImageMarkers("look [image:1] then [image:2] "); got != "look then" {
		t.Fatalf("stripImageMarkers = %q", got)
	}
	if got := userTextWithImages("look [image:1]", 1); got != "look [image:1]" {
		t.Fatalf("userTextWithImages should keep inline marker, got %q", got)
	}
	if got := userTextWithImages("", 2); got != "[2 images]" {
		t.Fatalf("empty image display fallback = %q", got)
	}
}

func TestUserPromptWrapsInHistory(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 24, Height: 20})
	out := m.renderLine(block{kind: bkUser, text: strings.Repeat("word ", 12)})
	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		rows = append(rows, line)
		if lipgloss.Width(line) > m.width {
			t.Fatalf("expected wrapped line width <= %d, got %d for %q", m.width, lipgloss.Width(line), line)
		}
	}
	if len(rows) < 2 {
		t.Fatalf("expected long user prompt to wrap, got %q", out)
	}
}

// TestUpdateNoPanic exercises the message handlers; it would panic if Model held
// a value-copied strings.Builder.
func TestUpdateNoPanic(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m = step(m, toolStartMsg{name: "read_file", args: `{"path":"go.mod"}`})
	m = step(m, toolEndMsg{name: "read_file", result: "module aigem\ngo 1.24"})
	m = step(m, contentMsg("hello "))
	m = step(m, contentMsg("world"))
	m = step(m, turnDoneMsg{answer: "hello world"})

	view := m.View()
	if !strings.Contains(view, "read_file") {
		t.Fatalf("expected tool call in view, got:\n%s", view)
	}
	if !hasBlock(m, bkAssistant, "hello world") {
		t.Fatalf("expected assistant answer persisted, got blocks: %+v", m.blocks)
	}
}

func TestModelPickerSwitchAndLock(t *testing.T) {
	// Isolated state/config so OpenAI is unauthenticated (locked).
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cwd := t.TempDir()
	reg, _ := tools.NewRegistry(cwd)
	skills, _ := skill.Discover(cwd)
	local := llm.LocalProvider("http://127.0.0.1:9280", "gemma.gguf", 262144, 8192)
	modelReg, _ := llm.NewRegistry(cwd, local)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9280", "gemma.gguf")), reg, 0.3,
		"local/gemma.gguf", "u", "", 262144, 8192, agent.DefaultSubagents(), "", skills, nil, nil, "",
		agent.CompactConfig{CtxSize: 262144}, modelReg)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// /model opens the picker; OpenAI entries are locked, local is not.
	m = typeEnter(m, "/model")
	if m.models == nil {
		t.Fatal("expected model picker open")
	}
	var sawLockedOpenAI, sawUnlockedLocal bool
	for _, it := range m.models.all {
		if it.provider == "openai" && it.locked {
			sawLockedOpenAI = true
		}
		if it.provider == "local" && !it.locked {
			sawUnlockedLocal = true
		}
	}
	if !sawLockedOpenAI || !sawUnlockedLocal {
		t.Fatalf("lock state wrong: openaiLocked=%v localUnlocked=%v", sawLockedOpenAI, sawUnlockedLocal)
	}

	// Filter to an OpenAI model and select it: locked entry routes to login
	// (busy set), not a model switch; the backend stays local.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("openai/gpt-5.6-sol")})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.models != nil {
		t.Fatal("picker should close on select")
	}
	if m.backend.Model().Ref() != "local/gemma.gguf" {
		t.Fatalf("locked select must not switch backend, got %q", m.backend.Model().Ref())
	}
	if !m.busy {
		t.Fatal("locked select should start a login (busy)")
	}
}

func TestModelSwitchUpdatesGauge(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-test") // authenticates openai via env

	cwd := t.TempDir()
	reg, _ := tools.NewRegistry(cwd)
	skills, _ := skill.Discover(cwd)
	local := llm.LocalProvider("http://127.0.0.1:9280", "gemma.gguf", 262144, 8192)
	modelReg, _ := llm.NewRegistry(cwd, local)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9280", "gemma.gguf")), reg, 0.3,
		"local/gemma.gguf", "u", "", 262144, 8192, agent.DefaultSubagents(), "", skills, nil, nil, "",
		agent.CompactConfig{CtxSize: 262144}, modelReg)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m.switchModel("openai/gpt-5.6-sol", true)
	if m.backend.Model().Ref() != "openai/gpt-5.6-sol" {
		t.Fatalf("backend not switched: %q", m.backend.Model().Ref())
	}
	if m.ctxSize != 400000 || m.compactCfg.CtxSize != 400000 {
		t.Fatalf("gauge/compaction window not updated: ctx=%d compact=%d", m.ctxSize, m.compactCfg.CtxSize)
	}
	if m.model != "openai/gpt-5.6-sol" {
		t.Fatalf("status model label = %q", m.model)
	}
	if got := config.LoadPrefs().Model; got != "openai/gpt-5.6-sol" {
		t.Fatalf("explicit switch should persist the model, got %q", got)
	}
	// A non-persisting switch (e.g. session restore) must not change the saved pref.
	m.switchModel("local/gemma.gguf", false)
	if got := config.LoadPrefs().Model; got != "openai/gpt-5.6-sol" {
		t.Fatalf("non-persisting switch changed the saved pref to %q", got)
	}
}

func TestModelSwitchUsesMaxTokenFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cwd := t.TempDir()
	proj := filepath.Join(cwd, ".aigem")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"providers":[{"id":"custom","base_url":"http://127.0.0.1:9","api":"openai-completions","auth":"none","models":[{"id":"no-cap","context_window":1234}]}]}`
	if err := os.WriteFile(filepath.Join(proj, "models.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, _ := tools.NewRegistry(cwd)
	skills, _ := skill.Discover(cwd)
	local := llm.LocalProvider("http://127.0.0.1:9280", "gemma.gguf", 262144, 8192)
	modelReg, _ := llm.NewRegistry(cwd, local)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9280", "gemma.gguf")), reg, 0.3,
		"local/gemma.gguf", "u", "", 262144, 777, agent.DefaultSubagents(), "", skills, nil, nil, "",
		agent.CompactConfig{CtxSize: 262144}, modelReg)

	m.switchModel("custom/no-cap", true)
	c, ok := m.backend.Get().(*llm.Client)
	if !ok {
		t.Fatalf("expected client backend, got %T", m.backend.Get())
	}
	if c.MaxTokens != 777 {
		t.Fatalf("MaxTokens = %d, want fallback 777", c.MaxTokens)
	}
}

func TestFormatArgsCollapsesContent(t *testing.T) {
	got := formatArgs("write_file", `{"path":"a.go","content":"package a\nfunc x(){}\n"}`)
	if !strings.Contains(got, "a.go") {
		t.Fatalf("path should be shown: %q", got)
	}
	if !strings.Contains(got, "lines") || strings.Contains(got, "package a") {
		t.Fatalf("content should collapse to a line count, got: %q", got)
	}

	// A single short arg (bash cmd) is shown verbatim.
	if got := formatArgs("bash", `{"cmd":"ls -la"}`); got != "ls -la" {
		t.Fatalf("expected verbatim cmd, got: %q", got)
	}
}

func TestAgentsBrowserAndConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cwd := t.TempDir()
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", nil, nil, nil, "", agent.CompactConfig{}, nil)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// /agents opens the browser: the four subagents plus the web-search entry.
	m = typeEnter(m, "/agents")
	if m.agentBr == nil || len(m.agentBr.items) != 5 {
		t.Fatalf("expected 5 agents, got %+v", m.agentBr)
	}
	last := m.agentBr.items[len(m.agentBr.items)-1]
	if last.name != "web-search" || !last.configurable {
		t.Fatalf("expected last item to be configurable web-search, got %+v", last)
	}

	// Move to web-search, open detail, then the config editor.
	for i := 0; i < 4; i++ {
		m = step(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter}) // detail
	if !m.agentBr.detail || !strings.Contains(m.View(), "Enter configure") {
		t.Fatal("expected configurable detail view")
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter}) // editor
	if !m.agentBr.editing {
		t.Fatal("expected the config editor to open")
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyDown}) // API key field
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")})
	if m.agentBr.keyBuf != "abc" {
		t.Fatalf("expected keyBuf abc, got %q", m.agentBr.keyBuf)
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyBackspace})
	if m.agentBr.keyBuf != "ab" || !strings.Contains(m.View(), "••") {
		t.Fatalf("expected masked keyBuf ab, got %q", m.agentBr.keyBuf)
	}

	// A failed verify keeps the editor open with an error and the typed key.
	m.applyAgentCfg(agentCfgDoneMsg{err: fmt.Errorf("bad key")})
	if !m.agentBr.editing || !strings.Contains(m.agentBr.status, "bad key") {
		t.Fatalf("editor should stay open on error, got %+v", m.agentBr)
	}

	// A successful save hot-swaps web_search into the live registry and advertises
	// it in the system prompt (it was absent at startup, so searchPrompted flips).
	if _, ok := m.reg.Get("web_search"); ok {
		t.Fatal("web_search should not be registered before configuration")
	}
	cfg := search.Config{Provider: search.ProviderBrave, Brave: &search.BraveConfig{APIKey: "k"}}
	m.applyAgentCfg(agentCfgDoneMsg{cfg: cfg})
	if m.agentBr.editing || m.agentBr.detail {
		t.Fatal("editor should close after a successful save")
	}
	if _, ok := m.reg.Get("web_search"); !ok {
		t.Fatal("web_search should be registered after configuration")
	}
	if !m.searchPrompted {
		t.Fatal("searchPrompted should be set after enabling web search")
	}
	if !strings.Contains(m.agent.Messages()[0].Content, "web_search tool") {
		t.Fatal("system prompt should advertise web_search after enabling")
	}
	if !hasBlock(m, bkNotice, "web search configured") {
		t.Fatal("expected a confirmation notice")
	}
}

func TestAgentsBrowserConfiguresBrowserSearch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", nil, nil, nil, "", agent.CompactConfig{}, nil)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 24})

	m = typeEnter(m, "/agents")
	for i := 0; i < 4; i++ {
		m = step(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter}) // detail
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter}) // editor
	if m.agentBr.provider != search.ProviderBrave {
		t.Fatalf("default editor provider = %q", m.agentBr.provider)
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyRight}) // switch provider to browser
	if m.agentBr.provider != search.ProviderBrowser {
		t.Fatalf("expected browser provider, got %q", m.agentBr.provider)
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyDown}) // engine (duckduckgo by default)
	m = step(m, tea.KeyMsg{Type: tea.KeyDown}) // optional profile

	cfg, err := m.agentBr.searchConfig()
	if err != nil {
		t.Fatalf("searchConfig: %v", err)
	}
	if cfg.Provider != search.ProviderBrowser || cfg.Browser.Engine != search.BrowserEngineDuckDuckGo {
		t.Fatalf("unexpected browser config: %+v", cfg)
	}
	if cfg.Browser.Executable != "" || !strings.HasSuffix(cfg.Browser.ProfileDir, filepath.Join("aigem", "browser-profile")) {
		t.Fatalf("expected auto-detected executable and default profile, got %+v", cfg.Browser)
	}
	m.applyAgentCfg(agentCfgDoneMsg{cfg: cfg})
	if _, ok := m.reg.Get("web_search"); !ok {
		t.Fatal("web_search should be registered after browser configuration")
	}
	if !strings.Contains(m.agent.Messages()[0].Content, "Browser-provider rules") {
		t.Fatal("system prompt should mention browser-provider rules")
	}
}

func TestAgentsBrowserClearsSelectedSearchProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", nil, nil, nil, "", agent.CompactConfig{}, nil)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 24})

	cfg := search.Config{
		Provider: search.ProviderBrowser,
		Brave:    &search.BraveConfig{APIKey: "k"},
		Browser:  &search.BrowserConfig{Engine: search.BrowserEngineGoogle},
	}
	if err := search.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if t := search.NewTool(cfg); t != nil {
		m.reg.Register(t)
	}
	if _, ok := m.reg.Get("web_search"); !ok {
		t.Fatal("precondition: web_search should be registered before clear")
	}
	m = typeEnter(m, "/agents")
	for i := 0; i < 4; i++ {
		m = step(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter}) // detail
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter}) // editor
	if m.agentBr.provider != search.ProviderBrowser {
		t.Fatalf("expected browser provider from saved config, got %q", m.agentBr.provider)
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyDown}) // engine
	m = step(m, tea.KeyMsg{Type: tea.KeyDown}) // profile
	m = step(m, tea.KeyMsg{Type: tea.KeyDown}) // clear provider
	cmd := m.handleAgentEditKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected clear command")
	}
	msg := cmd().(agentCfgDoneMsg)
	m.applyAgentCfg(msg)

	saved, err := search.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Provider != search.ProviderBrave || saved.Brave == nil || saved.Browser != nil {
		t.Fatalf("expected browser removed and brave left active, got %+v", saved)
	}
	if _, ok := m.reg.Get("web_search"); !ok {
		t.Fatal("web_search should stay registered using the remaining provider")
	}
	if !hasBlock(m, bkNotice, "browser search settings cleared") {
		t.Fatal("expected provider clear confirmation notice")
	}
}

func TestNewSessionClears(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", nil, nil, nil, "", agent.CompactConfig{}, nil)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Run a turn (dead port: it fails fast) to populate blocks, session, history.
	m = typeEnter(m, "hello")
	for {
		msg := <-m.events
		m = step(m, msg)
		if _, ok := msg.(turnDoneMsg); ok {
			break
		}
	}
	m = step(m, todoUpdateMsg{{Text: "step", Status: agent.TodoPending}})
	m.toolPolicy["bash"] = "allow"
	savedID := m.sessionID

	if len(m.blocks) == 0 || m.sessionID == "" || len(m.history) == 0 || len(m.todos) == 0 {
		t.Fatalf("precondition: expected populated state, got blocks=%d session=%q history=%d todos=%d",
			len(m.blocks), m.sessionID, len(m.history), len(m.todos))
	}

	// /new wipes everything back to a fresh start.
	m = typeEnter(m, "/new")
	if len(m.toolPolicy) != 0 {
		t.Fatalf("tool policy should be cleared, got %+v", m.toolPolicy)
	}
	// The prior session is persisted first, so it stays resumable.
	if _, err := session.Load(savedID); err != nil {
		t.Fatalf("prior session should be saved before /new: %v", err)
	}
	if len(m.blocks) != 0 {
		t.Fatalf("blocks should be cleared, got %+v", m.blocks)
	}
	if m.sessionID != "" || m.sessionTitle != "" || !m.sessionStart.IsZero() {
		t.Fatal("session identity should be cleared")
	}
	if len(m.todos) != 0 || m.ctxTokens != 0 || len(m.history) != 0 {
		t.Fatalf("todos/ctx/history should be cleared: todos=%d ctx=%d history=%d",
			len(m.todos), m.ctxTokens, len(m.history))
	}
	if msgs := m.agent.Messages(); len(msgs) != 1 || msgs[0].Role != llm.RoleSystem {
		t.Fatalf("agent should hold only the system prompt, got %+v", msgs)
	}
	if !strings.Contains(m.View(), "minimal local coding agent") {
		t.Fatal("the welcome banner should be shown after /new")
	}
}

func TestNewSessionRebuildsSystemPrompt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "LAUNCH PROMPT", 8192, 8192,
		agent.DefaultSubagents(), "", nil, nil, nil, "", agent.CompactConfig{}, nil)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	rebuilt := "LAUNCH PROMPT"
	m.SetSystemRebuilder(func() string { return rebuilt })

	if got := m.agent.Messages()[0].Content; got != "LAUNCH PROMPT" {
		t.Fatalf("expected launch prompt, got %q", got)
	}

	// An edit to project instructions lands only on the next /new.
	rebuilt = "REBUILT PROMPT with context.md"
	m = typeEnter(m, "/new")

	if got := m.agent.Messages()[0].Content; got != "REBUILT PROMPT with context.md" {
		t.Fatalf("/new should rebuild the system prompt, got %q", got)
	}
}

func hasBlock(m Model, kind blockKind, text string) bool {
	for _, b := range m.blocks {
		if b.kind == kind && strings.Contains(b.text, text) {
			return true
		}
	}
	return false
}

func TestToolOutputToggle(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = step(m, toolStartMsg{name: "read_file", args: `{"path":"go.mod"}`})
	m = step(m, toolEndMsg{name: "read_file", result: "module aigem\ngo 1.24"})

	if strings.Contains(m.View(), "module aigem") {
		t.Fatal("tool output should be hidden by default")
	}
	// Ctrl+O toggles it on.
	m = step(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if !strings.Contains(m.View(), "module aigem") {
		t.Fatal("tool output should be visible after toggle")
	}
	// The call line is always shown regardless of the toggle.
	m = step(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if !strings.Contains(m.View(), "read_file") {
		t.Fatal("tool call line should always be visible")
	}

	// Errors are always shown even when output is hidden, so a failed tool
	// never looks like a success.
	if m.showToolOutput {
		t.Fatal("expected output hidden at this point")
	}
	m = step(m, toolEndMsg{name: "write_file", err: errWrite})
	if !strings.Contains(m.View(), "boom") {
		t.Fatal("tool errors must be visible even when output is hidden")
	}
}

func TestPlanSidebar(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 24})

	if m.sidebarVisible() {
		t.Fatal("sidebar should be hidden without a plan")
	}

	m = step(m, todoUpdateMsg{
		{Text: "read the code", Status: agent.TodoCompleted},
		{Text: "make the change", Status: agent.TodoInProgress},
		{Text: "run the tests", Status: agent.TodoPending},
	})
	if !m.sidebarVisible() {
		t.Fatal("sidebar should appear once a plan exists")
	}

	// Expanded: header + items, each row exactly sidebarWidth wide, content-height
	// (no padding to the full viewport).
	side := m.sidebarLines()
	if len(side) >= m.vp.Height {
		t.Fatalf("expanded panel should be content-height, got %d rows for vp %d", len(side), m.vp.Height)
	}
	joined := strings.Join(side, "\n")
	if !strings.Contains(joined, "▾ Plan  1/3") {
		t.Fatalf("panel should show the chevron and count, got:\n%s", joined)
	}
	if !strings.Contains(joined, "make the change") {
		t.Fatal("panel should list plan items")
	}
	for _, ln := range side {
		if w := lipgloss.Width(ln); w != sidebarWidth {
			t.Fatalf("every panel row must be exactly %d wide, got %d: %q", sidebarWidth, w, ln)
		}
	}

	// The overlay keeps the composed chat lines at full width.
	for _, ln := range strings.Split(m.overlaySidebar(m.vp.View()), "\n")[:len(side)] {
		if w := lipgloss.Width(ln); w != m.width {
			t.Fatalf("overlaid chat line should be full width %d, got %d", m.width, w)
		}
	}

	// Ctrl+T collapses to just the framed title (top + title + bottom).
	m = step(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if !m.todoCollapsed {
		t.Fatal("Ctrl+T should collapse the panel")
	}
	if c := m.sidebarLines(); len(c) != 3 || !strings.Contains(c[1], "▸ Plan  1/3") {
		t.Fatalf("collapsed panel should be a 3-row framed title with a ▸ chevron, got %#v", c)
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	if m.todoCollapsed {
		t.Fatal("Ctrl+T should expand the panel again")
	}

	// A narrow terminal hides the panel even with a plan.
	m = step(m, tea.WindowSizeMsg{Width: 70, Height: 24})
	if m.sidebarVisible() {
		t.Fatal("panel should be suppressed on a narrow terminal")
	}
}

var errWrite = stubErr("boom: path escapes the sandbox root")

type stubErr string

func (e stubErr) Error() string { return string(e) }

func TestConfirmFlow(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	resp := make(chan bool, 1)
	m = step(m, confirmReqMsg{name: "bash", args: `{"cmd":"ls"}`, resp: resp})
	if m.pending == nil {
		t.Fatal("expected pending confirmation")
	}
	if !strings.Contains(m.View(), "Once") {
		t.Fatal("expected confirm box in view")
	}

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if m.pending != nil {
		t.Fatal("expected confirmation cleared after answer")
	}
	if got := <-resp; got != true {
		t.Fatal("expected approval sent to agent")
	}
}

// TestOverlayBlursInput verifies the chat input loses focus (so its cursor
// disappears) while a modal dialog is open, regains it once the dialog closes,
// and stays focused for the input-attached command menu.
func TestOverlayBlursInput(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("draft")})
	if !m.input.Focused() {
		t.Fatal("input should be focused while typing")
	}

	resp := make(chan bool, 1)
	m = step(m, confirmReqMsg{name: "bash", args: `{"cmd":"ls"}`, resp: resp})
	if m.input.Focused() {
		t.Fatal("input should be blurred while a modal dialog is open")
	}

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	<-resp
	if !m.input.Focused() {
		t.Fatal("input should regain focus after the dialog closes")
	}

	// The command menu is an autocomplete popup driven by the input text, so the
	// input must stay focused (and editable) while it is open.
	m.input.Reset()
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if m.cmdMenu == nil {
		t.Fatal("expected command menu to open")
	}
	if !m.input.Focused() {
		t.Fatal("input should stay focused while the command menu is open")
	}
}

// TestSubagentGrouping verifies that concurrent subagent runs stay grouped and
// attributed by their parent-call id rather than interleaving into one stream.
// TestScrollStickyBottom verifies the viewport follows the bottom only while
// already there, preserves position when the user scrolls up, and re-anchors on
// a new message.
func TestScrollStickyBottom(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	for i := 0; i < 60; i++ {
		m = step(m, toolStartMsg{name: "read_file", args: `{"path":"f.go"}`})
	}
	if !m.vp.AtBottom() {
		t.Fatal("should follow the bottom as content streams in")
	}

	// Scroll up a page.
	m = step(m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.vp.AtBottom() {
		t.Fatal("PgUp should scroll up off the bottom")
	}

	// New content arrives - position must be preserved, not yanked to bottom.
	m = step(m, toolStartMsg{name: "grep", args: `{"pattern":"x"}`})
	if m.vp.AtBottom() {
		t.Fatal("new content must not force-scroll to bottom while scrolled up")
	}

	// Sending a message re-anchors to the bottom.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.vp.AtBottom() {
		t.Fatal("submitting a message should re-anchor to the bottom")
	}
}

// TestResumedSessionScrollable verifies that a session restored via /resume is
// rendered into the viewport, anchored at the bottom, and scrollable.
func TestResumedSessionScrollable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	msgs := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}
	for i := 0; i < 60; i++ {
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("question %d", i)})
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("answer %d", i)})
	}
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s := &session.Session{
		Meta:     session.Meta{ID: session.NewID(now), Title: "old chat", Created: now},
		Messages: msgs,
	}
	if err := session.Save(s, now); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Type /resume and open the picker, then load the (only) session.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/resume")})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.picker == nil {
		t.Fatal("/resume should open the session picker")
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.picker != nil {
		t.Fatal("picker should close after selecting a session")
	}

	if len(m.blocks) == 0 {
		t.Fatal("restored session should populate the conversation")
	}
	if !m.vp.AtBottom() {
		t.Fatal("resumed session should anchor at the bottom")
	}
	// The restored history must be scrollable.
	m = step(m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.vp.AtBottom() {
		t.Fatal("PgUp should scroll through the restored history")
	}
}

func TestSubagentGrouping(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m = step(m, agentStartMsg{id: "1", agent: "scout", prompt: "find A"})
	m = step(m, agentStartMsg{id: "2", agent: "reviewer", prompt: "review B"})
	// Events arrive interleaved across the two runs.
	m = step(m, subToolStartMsg{id: "1", agent: "scout", name: "grep", args: `{"pattern":"x"}`})
	m = step(m, subToolStartMsg{id: "2", agent: "reviewer", name: "read_file", args: `{"path":"y"}`})
	m = step(m, subToolStartMsg{id: "1", agent: "scout", name: "list_dir", args: `{"path":"."}`})
	m = step(m, agentEndMsg{id: "1"})

	if g := m.groups["1"]; g == nil || len(g.lines) != 2 || !g.done {
		t.Fatalf("scout group wrong: %+v", m.groups["1"])
	}
	if g := m.groups["2"]; g == nil || len(g.lines) != 1 || g.done {
		t.Fatalf("reviewer group wrong: %+v", m.groups["2"])
	}
	view := m.View()
	for _, want := range []string{"scout", "reviewer", "grep", "read_file", "list_dir"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

// TestConfirmQueue verifies that concurrent confirmations queue behind one box
// and promote in turn.
func TestConfirmQueue(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	r1 := make(chan bool, 1)
	r2 := make(chan bool, 1)
	m = step(m, confirmReqMsg{name: "bash", args: `{"cmd":"ls"}`, resp: r1})
	m = step(m, confirmReqMsg{name: "write_file", args: `{"path":"a"}`, resp: r2})

	if m.pending == nil || m.pending.name != "bash" {
		t.Fatal("expected bash confirm shown first")
	}
	if len(m.pendingQueue) != 1 {
		t.Fatalf("expected one queued confirm, got %d", len(m.pendingQueue))
	}

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if got := <-r1; !got {
		t.Fatal("expected bash approved")
	}
	if m.pending == nil || m.pending.name != "write_file" {
		t.Fatal("expected write_file promoted after answering bash")
	}
	if len(m.pendingQueue) != 0 {
		t.Fatal("queue should be empty")
	}
	step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if got := <-r2; !got {
		t.Fatal("expected write_file approved")
	}
}

// TestConfirmAlwaysPolicy approves a tool for the session, after which later
// calls to the same tool are auto-approved without a prompt.
func TestConfirmAlwaysPolicy(t *testing.T) {
	m := newTestModel(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	resp := make(chan bool, 1)
	m = step(m, confirmReqMsg{name: "bash", args: `{"cmd":"ls"}`, resp: resp})
	// Select "Always" and confirm.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if got := <-resp; got != true {
		t.Fatal("expected approval")
	}
	if m.toolPolicy["bash"] != "allow" {
		t.Fatalf("expected session allow policy, got %q", m.toolPolicy["bash"])
	}

	// A second bash request must auto-approve without showing the box.
	resp2 := make(chan bool, 1)
	m = step(m, confirmReqMsg{name: "bash", args: `{"cmd":"pwd"}`, resp: resp2})
	if m.pending != nil {
		t.Fatal("expected no prompt for an always-allowed tool")
	}
	if got := <-resp2; got != true {
		t.Fatal("expected auto-approval from session policy")
	}
}

func TestStableSplit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"no boundary yet", "# Title\nsome text", 0},
		{"trailing newline, no blank", "# Title\n", 0},
		{"one closed block", "# Title\n\ntail", len("# Title\n\n")},
		{"open fence keeps tail plain", "intro\n\n```go\nfunc main() {}\n", len("intro\n\n")},
		{"closed fence then blank", "```go\nx := 1\n```\n\nnext", len("```go\nx := 1\n```\n\n")},
		{"blank inside fence ignored", "```\na\n\nb\n```\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stableSplit(c.in); got != c.want {
				t.Fatalf("stableSplit(%q) = %d, want %d", c.in, got, c.want)
			}
			if got := stableSplit(c.in); got < 0 || got > len(c.in) {
				t.Fatalf("stableSplit(%q) = %d out of range [0,%d]", c.in, got, len(c.in))
			}
		})
	}
}

func TestCatppuccinStyleNoBackgroundGaps(t *testing.T) {
	st := buildCatppuccinStyle(cCard)
	for i, p := range prosePrimitives(&st) {
		if p.BackgroundColor == nil {
			t.Errorf("prose primitive %d has no background: a gap would show through the card", i)
		}
	}
	for i, p := range chromaPrimitives(st.CodeBlock.Chroma) {
		if p.BackgroundColor == nil {
			t.Errorf("chroma primitive %d has no background: a gap would show in code blocks", i)
		}
	}
}

func TestSplitCodeBlocks(t *testing.T) {
	segs := splitCodeBlocks("intro\n\n```go\nx := 1\n```\n\nouttro\n")
	if len(segs) != 3 {
		t.Fatalf("want 3 segments, got %d: %+v", len(segs), segs)
	}
	if segs[0].code || !segs[1].code || segs[2].code {
		t.Fatalf("expected prose/code/prose, got %v/%v/%v", segs[0].code, segs[1].code, segs[2].code)
	}
	if !strings.Contains(segs[1].text, "x := 1") || !strings.HasPrefix(strings.TrimSpace(segs[1].text), "```") {
		t.Fatalf("code segment missing fence or body: %q", segs[1].text)
	}
	// Unclosed trailing fence is treated as code.
	open := splitCodeBlocks("text\n\n```go\nhalf")
	if last := open[len(open)-1]; !last.code {
		t.Fatalf("unclosed fence should be code: %+v", last)
	}
	// No fences => single prose segment.
	if got := splitCodeBlocks("just text\n"); len(got) != 1 || got[0].code {
		t.Fatalf("plain text should be one prose segment, got %+v", got)
	}
}

func TestLocalWizardProducesConfig(t *testing.T) {
	w := &localWizard{}
	w.init() // step 0, defaults loaded

	// Step 0: keep default source (HF) -> Enter.
	w.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	// Step 1: clear and type a custom repo.
	for range w.value {
		w.handleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	for _, r := range "me/repo:Q4" {
		w.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	w.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	// Step 2: keep default binary -> Enter.
	w.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	cfg := w.config()
	if cfg.SourceKind != local.SourceHF {
		t.Errorf("SourceKind = %q", cfg.SourceKind)
	}
	if cfg.HFRepo != "me/repo:Q4" {
		t.Errorf("HFRepo = %q", cfg.HFRepo)
	}
	if cfg.BinaryPath != "llama-server" {
		t.Errorf("BinaryPath = %q", cfg.BinaryPath)
	}
	if w.step != wizStepConfirm {
		t.Errorf("step = %d, want confirm", w.step)
	}
}

func TestAssessActiveModelLocalNotSetUp(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // no local.json -> not set up
	a := assessActiveModel(llm.LocalProviderID + "/whatever.gguf")
	if a == nil {
		t.Fatal("expected a warning for an unconfigured local model")
	}
	if a.action != alertLocalSetup {
		t.Errorf("action = %d, want alertLocalSetup", a.action)
	}
	if a.confirmLabel == "" {
		t.Error("expected a confirm button label")
	}
}

func TestAssessActiveModelRemoteUnauthed(t *testing.T) {
	// A non-OpenAI provider has no interactive login, so the box is dismiss-only
	// and points to the CLI `aigem auth login` path.
	a := assessActiveModel("definitely-not-a-real-provider/m")
	if a == nil {
		t.Fatal("expected a warning for an unauthenticated provider")
	}
	if a.action != alertDismiss || a.confirmLabel != "" {
		t.Errorf("non-OpenAI provider should be dismiss-only, got action=%d label=%q", a.action, a.confirmLabel)
	}
	if !strings.Contains(a.body, "auth login") {
		t.Errorf("body should guide to CLI login, got %q", a.body)
	}
}

func TestQuotaSegment(t *testing.T) {
	m := newTestModel(t)

	// Nothing is shown until a response has actually carried quota headers.
	if s, _ := m.quotaSegment(); s != "" {
		t.Fatalf("segment before any call = %q", s)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range map[string]string{
			"X-Codex-Primary-Used-Percent":                  "3",
			"X-Codex-Primary-Window-Minutes":                "10080",
			"X-Codex-Bengalfox-Limit-Name":                  "Spark",
			"X-Codex-Bengalfox-Primary-Used-Percent":        "71",
			"X-Codex-Bengalfox-Primary-Window-Minutes":      "10080",
			"X-Codex-Bengalfox-Primary-Reset-After-Seconds": "7200",
		} {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := llm.NewClient(llm.ClientConfig{BaseURL: srv.URL, Info: llm.ModelInfo{Provider: "openai", ID: "gpt-5.6-sol"}})
	m.backend.Set(c)
	if _, err := c.Stream(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		nil, 0, func(llm.StreamEvent) {}); err != nil {
		t.Fatal(err)
	}

	// The tightest window wins, not the account-wide one: 71% is what stops work.
	s, pct := m.quotaSegment()
	if !strings.HasPrefix(s, "quota 71%") || pct != 71 {
		t.Fatalf("segment = %q, pct = %v", s, pct)
	}
	if !strings.Contains(s, "1h5") && !strings.Contains(s, "2h") {
		t.Fatalf("segment %q should say when the window resets (~2h)", s)
	}
	if !strings.Contains(m.statusLine(), "quota 71%") {
		t.Fatalf("status line is missing the quota segment: %q", m.statusLine())
	}
}

// untrustedSkillProject writes a project-local skill without approving it, so
// discovery withholds it exactly as it does on a first launch.
func untrustedSkillProject(t *testing.T, name, content string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestSkillTrustPromptApprovesAndLoads(t *testing.T) {
	cwd := untrustedSkillProject(t, "gitea",
		"---\nname: gitea\ndescription: read tracker issues\n---\nCall the API.\n")
	skills, _ := skill.Discover(cwd)
	if skills.Len() != 0 {
		t.Fatalf("precondition: untrusted skill must be withheld, got %d", skills.Len())
	}
	pending, err := skill.Pending(cwd)
	if err != nil || pending == nil {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m.SetSystemRebuilder(func() string { return "sys\n\n" + skills.Prompt() })
	m.SetPendingSkills(cwd, pending)
	// Wide enough for the temp path to be shown in full: the overlay truncates
	// Dir to fit, and t.TempDir() is far longer on some platforms (macOS hands
	// back /var/folders/...) than the 80 columns a narrower window would give.
	m = step(m, tea.WindowSizeMsg{Width: len(cwd) + 40, Height: 24})

	// The prompt is a modal: it owns the keyboard and names the withheld skill.
	view := m.View()
	for _, want := range []string{"Load this project's skills?", cwd, "1 skill(s):", "gitea",
		"pre-approve tools", "y trust (persisted) · ctrl+c quit · any other key skip"} {
		if !strings.Contains(view, want) {
			t.Fatalf("skill trust overlay is missing %q: %s", want, view)
		}
	}
	if !m.anyOverlayOpen() || m.input.Focused() {
		t.Error("the trust prompt must be modal and blur the input")
	}

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if m.skillAsk != nil {
		t.Error("prompt must close after a decision")
	}
	if m.skills.Len() != 1 {
		t.Fatalf("approved skill not loaded, got %d", m.skills.Len())
	}
	// The skill tool did not exist at launch (no trusted skills) and must now.
	if _, ok := reg.Get(agent.SkillToolName); !ok {
		t.Error("skill tool not registered after approval")
	}
	msgs := m.agent.Messages()
	if len(msgs) == 0 || msgs[0].Role != llm.RoleSystem || !strings.Contains(msgs[0].Content, "gitea") {
		t.Error("system prompt was not rebuilt with the newly trusted skill")
	}
	var hasCmd bool
	for _, c := range m.commands {
		if c.name == "/skill:gitea" {
			hasCmd = true
		}
	}
	if !hasCmd {
		t.Error("command menu was not rebuilt with the newly trusted skill")
	}
	// Approval is persisted, so the next launch discovers it without asking.
	again, _ := skill.Discover(cwd)
	if again.Len() != 1 {
		t.Errorf("approval was not persisted, next discovery found %d", again.Len())
	}
	if p, _ := skill.Pending(cwd); p != nil {
		t.Errorf("nothing should stay pending after approval, got %+v", p)
	}
}

func TestSkillTrustPromptDeclineLeavesSkillsOut(t *testing.T) {
	cwd := untrustedSkillProject(t, "gitea",
		"---\nname: gitea\ndescription: read tracker issues\n---\nCall the API.\n")
	skills, _ := skill.Discover(cwd)
	pending, _ := skill.Pending(cwd)
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m.SetPendingSkills(cwd, pending)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})

	if m.skillAsk != nil {
		t.Error("prompt must close after a decision")
	}
	if m.skills.Len() != 0 {
		t.Errorf("declined skills must stay out, got %d", m.skills.Len())
	}
	if !strings.Contains(m.View(), "restart to be asked again") {
		t.Errorf("declining must say how to get asked again, and fit the terminal: %s", m.View())
	}
	// Declining is not persisted as a denial: the next launch asks again.
	if p, _ := skill.Pending(cwd); p == nil {
		t.Error("a skipped prompt must still be pending on the next launch")
	}
}

func TestHookTrustPromptPrecedesSkillTrustPrompt(t *testing.T) {
	cwd := untrustedSkillProject(t, "gitea",
		"---\nname: gitea\ndescription: read tracker issues\n---\nCall the API.\n")
	settings := filepath.Join(cwd, ".claude")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	hookCfg := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo hi"}]}]}}`
	if err := os.WriteFile(filepath.Join(settings, "settings.json"), []byte(hookCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	runner, _ := hooks.Load(cwd)
	if !runner.HasUntrustedProjectHooks() {
		t.Skip("project hooks are not treated as untrusted here")
	}
	skills, _ := skill.Discover(cwd)
	pending, _ := skill.Pending(cwd)
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, runner, nil, "", agent.CompactConfig{}, nil)
	m.SetPendingSkills(cwd, pending)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Both are armed; the hook prompt renders first, then the skill one.
	if !strings.Contains(m.View(), "Trust this project's hooks?") {
		t.Fatalf("hook prompt should come first: %s", m.View())
	}
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if !strings.Contains(m.View(), "Load this project's skills?") {
		t.Fatalf("skill prompt should follow the hook one: %s", m.View())
	}
}

// A project that also has a trusted global skill already had the skill tool
// registered at launch. That is the path where the in-place registry swap
// carries the work, since the tool holds the pointer it was built with.
func TestSkillTrustApprovalReachesAlreadyRegisteredSkillTool(t *testing.T) {
	cwd := untrustedSkillProject(t, "gitea",
		"---\nname: gitea\ndescription: read tracker issues\n---\nCall the API.\n")
	global := filepath.Join(os.Getenv("HOME"), ".claude", "skills", "greet")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "SKILL.md"),
		[]byte("---\nname: greet\ndescription: say hi\n---\nHello.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skills, _ := skill.Discover(cwd)
	if skills.Len() != 1 {
		t.Fatalf("precondition: only the global skill should load, got %d", skills.Len())
	}
	pending, _ := skill.Pending(cwd)
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m.SetPendingSkills(cwd, pending)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	st, ok := reg.Get(agent.SkillToolName)
	if !ok {
		t.Fatal("precondition: the skill tool should exist at launch")
	}
	toolCount := len(reg.Names())
	if strings.Contains(st.Description(), "gitea") {
		t.Fatal("precondition: the untrusted skill must not be advertised yet")
	}

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	// Same tool instance: it must see the new set through the shared pointer.
	if !strings.Contains(st.Description(), "gitea") {
		t.Errorf("the live skill tool did not pick up the approved skill: %s", st.Description())
	}
	if !strings.Contains(string(st.Schema()), "gitea") {
		t.Errorf("the skill tool enum was not refreshed: %s", st.Schema())
	}
	if got := len(reg.Names()); got != toolCount {
		t.Errorf("re-registration changed the tool count: %d, want %d", got, toolCount)
	}
}

func TestSkillTrustApprovalRearmsConditionalSkills(t *testing.T) {
	cwd := untrustedSkillProject(t, "golang",
		"---\nname: golang\ndescription: go conventions\npaths: [\"**/*.go\"]\n---\nUse gofmt.\n")
	skills, _ := skill.Discover(cwd)
	pending, _ := skill.Pending(cwd)
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m.SetPendingSkills(cwd, pending)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if got := m.skills.Conditional(); len(got) != 1 || got[0].Name != "golang" {
		t.Fatalf("conditional skill not loaded: %+v", got)
	}
	// A conditional skill is deliberately absent from the always-on listing; it
	// only ever reaches the model through the agent's path watch.
	if strings.Contains(m.skills.Prompt(), "golang") {
		t.Error("a path-gated skill must stay out of the always-on listing")
	}
	watched := m.agent.WatchedSkills()
	if len(watched) != 1 || watched[0].Name != "golang" {
		t.Errorf("the agent was not re-armed to watch the approved conditional skill: %+v", watched)
	}
}

func TestSkillTrustApprovalFailureSurfaces(t *testing.T) {
	cwd := untrustedSkillProject(t, "gitea",
		"---\nname: gitea\ndescription: read tracker issues\n---\nCall the API.\n")
	skills, _ := skill.Discover(cwd)
	pending, _ := skill.Pending(cwd)
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m.SetPendingSkills(cwd, pending)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Persisting the approval must fail: point the state dir at a regular file.
	blocked := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blocked)

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if m.skills.Len() != 0 {
		t.Errorf("nothing should load when the approval could not be persisted, got %d", m.skills.Len())
	}
	if !strings.Contains(m.View(), "could not trust project skills") {
		t.Errorf("the failure was not surfaced: %s", m.View())
	}
}

func TestSkillTrustPromptCtrlCQuits(t *testing.T) {
	cwd := untrustedSkillProject(t, "gitea",
		"---\nname: gitea\ndescription: read tracker issues\n---\nCall the API.\n")
	skills, _ := skill.Discover(cwd)
	pending, _ := skill.Pending(cwd)
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m.SetPendingSkills(cwd, pending)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c at the trust prompt must quit, not silently decline")
	}
	if !yieldsQuit(cmd) {
		t.Fatalf("expected a quit command, got %T", cmd())
	}
}

// yieldsQuit reports whether cmd produces a quit, directly or inside a batch.
func yieldsQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case tea.QuitMsg:
		return true
	case tea.BatchMsg:
		for _, c := range msg {
			if yieldsQuit(c) {
				return true
			}
		}
	}
	return false
}

func TestSkillTrustNameListSummarizesOverflow(t *testing.T) {
	var names []string
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("a-fairly-long-skill-name-%02d", i))
	}
	if lines := skillTrustNameLines(names, 76, 4); len(lines) != 4 {
		t.Fatalf("lines = %d, want 4", len(lines))
	}
	assertOverflow(t, names, 76)
	if got := skillTrustNameLines(nil, 76, 4); len(got) != 1 || !strings.Contains(got[0], "no readable") {
		t.Errorf("an empty name list must say so: %v", got)
	}
}

// assertOverflow checks the layout invariant the trust prompt depends on: every
// line fits the box in terminal cells, and the names actually rendered plus the
// number the footer admits to hiding add up to the whole list. Names are matched
// by membership rather than by splitting on the separator, since a name may
// contain the separator - which is exactly the case that must still add up.
func assertOverflow(t *testing.T, names []string, w int) {
	t.Helper()
	lines := skillTrustNameLines(names, w, 4)
	for _, l := range lines {
		if ansi.StringWidth(l) > w {
			t.Errorf("line overflows the overlay (%d cells > %d): %q", ansi.StringWidth(l), w, l)
		}
	}
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "... and ") || !strings.HasSuffix(last, " more") {
		t.Fatalf("overflow must be summarized, not cut: %q", last)
	}
	var hidden int
	if _, err := fmt.Sscanf(last, "... and %d more", &hidden); err != nil {
		t.Fatalf("unparsable footer %q: %v", last, err)
	}
	shownText := strings.Join(lines[:len(lines)-1], "\n")
	if strings.Contains(shownText, "…") {
		t.Fatalf("fixture names should not need truncating at w=%d: %q", w, shownText)
	}
	shown := 0
	for _, n := range names {
		if strings.Contains(shownText, n) {
			shown++
		}
	}
	if shown+hidden != len(names) {
		t.Errorf("%d rendered + %d admitted hidden = %d, want %d names accounted for",
			shown, hidden, shown+hidden, len(names))
	}
}

// A name may contain the separator or be double width. Neither may let the count
// drift: the footer is the only claim in the box a crafted name cannot forge.
func TestSkillTrustNameListCountsHostileNames(t *testing.T) {
	commas := make([]string, 20)
	for i := range commas {
		commas[i] = fmt.Sprintf("aaaa, bbbb, cccc-%02d", i)
	}
	assertOverflow(t, commas, 76)

	wide := make([]string, 30)
	for i := range wide {
		wide[i] = fmt.Sprintf("スキル名前%02d", i)
	}
	assertOverflow(t, wide, 76)

	spaced := make([]string, 30)
	for i := range spaced {
		spaced[i] = fmt.Sprintf("name with spaces %02d", i)
	}
	assertOverflow(t, spaced, 76)
}

// One absurdly long name must not push the y/skip hint off the screen: that hint
// is how the user declines, and the name is chosen by the repo being judged.
func TestSkillTrustOverlayHeightIsBounded(t *testing.T) {
	cwd := untrustedSkillProject(t, "huge",
		"---\nname: \""+strings.Repeat("A", 4000)+"\"\ndescription: d\n---\nbody\n")
	skills, _ := skill.Discover(cwd)
	pending, err := skill.Pending(cwd)
	if err != nil || pending == nil {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m.SetPendingSkills(cwd, pending)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	for _, l := range strings.Split(view, "\n") {
		if ansi.StringWidth(l) > 80 {
			t.Fatalf("view line is %d cells wide, terminal is 80: %q", ansi.StringWidth(l), l)
		}
	}
	if n := len(strings.Split(view, "\n")); n > 24 {
		t.Errorf("view is %d rows, terminal is 24: the trust hint would scroll away", n)
	}
	if !strings.Contains(view, "any other key skip") {
		t.Error("the decline hint must stay on screen")
	}
}

// The invalidated variant is what a user sees after a `git pull` rewrites a
// skill, so it has to say that rather than look like a first-time prompt.
func TestSkillTrustPromptReportsChangedDefinitions(t *testing.T) {
	cwd := untrustedSkillProject(t, "gitea",
		"---\nname: gitea\ndescription: read tracker issues\n---\nv1\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, ".skills", "gitea", "SKILL.md")
	if err := os.WriteFile(path, []byte("---\nname: gitea\ndescription: read tracker issues\n---\nv2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skills, _ := skill.Discover(cwd)
	pending, _ := skill.Pending(cwd)
	if pending == nil || !pending.Invalidated {
		t.Fatalf("pending = %+v, want an invalidated approval", pending)
	}
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m.SetPendingSkills(cwd, pending)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	view := m.View()
	for _, want := range []string{"Reload this project's changed skills?", "changed since you approved them"} {
		if !strings.Contains(view, want) {
			t.Errorf("invalidated overlay is missing %q: %s", want, view)
		}
	}
}

// A name that only sanitizes safely on the way in must not come back raw on the
// way out: the approval notice renders on the same screen, one keystroke later.
func TestSkillTrustApprovalNoticeSanitizesNames(t *testing.T) {
	cwd := untrustedSkillProject(t, "evil",
		"---\nname: \"be\\u200bnign\\e[31m\"\ndescription: d\n---\nbody\n")
	skills, _ := skill.Discover(cwd)
	pending, _ := skill.Pending(cwd)
	reg, _ := tools.NewRegistry(cwd)
	m := New(llm.NewRef(llm.New("http://127.0.0.1:9", "t")), reg, 0.3, "t", "u", "sys", 8192, 8192,
		agent.DefaultSubagents(), "", skills, nil, nil, "", agent.CompactConfig{}, nil)
	m.SetPendingSkills(cwd, pending)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if m.skills.Len() != 1 {
		t.Fatalf("the skill should still load, got %d", m.skills.Len())
	}
	if !strings.Contains(m.View(), "project skills trusted and loaded:") {
		t.Fatalf("approval notice not shown: %s", m.View())
	}
	// Strip the styling the TUI emits itself, then check nothing the skill chose
	// survived: the escape is gone and its inert residue is all that is left.
	plain := ansiRE.ReplaceAllString(m.View(), "")
	if strings.ContainsRune(plain, 0x1b) {
		t.Error("an escape from the skill name reached the terminal after approval")
	}
	if strings.ContainsRune(plain, 0x200b) {
		t.Error("a zero-width space from the skill name reached the terminal after approval")
	}
	if !strings.Contains(plain, "benign[31m") {
		t.Errorf("the defanged name should still be shown in full: %s", plain)
	}
}
