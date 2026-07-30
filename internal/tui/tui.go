// Package tui is a Bubble Tea front-end for the agent.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/sahilm/fuzzy"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/local"
	"github.com/gigovich/aigem/internal/mcp"
	"github.com/gigovich/aigem/internal/pathgrant"
	"github.com/gigovich/aigem/internal/session"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

// Catppuccin Mocha palette (https://catppuccin.com/palette). cBase is pinned to
// the mantle shade (not the lighter #1e1e2e base) so the whole frame reads dark
// and flat; the chat, markdown prose, input box, and plan panel all share it.
const (
	cBase     = lipgloss.Color("#181825")
	cMantle   = lipgloss.Color("#181825")
	cCrust    = lipgloss.Color("#11111b")
	cSurface0 = lipgloss.Color("#313244")
	cSurface1 = lipgloss.Color("#45475a")
	cSurface2 = lipgloss.Color("#585b70")
	cOverlay0 = lipgloss.Color("#6c7086")
	cOverlay1 = lipgloss.Color("#7f849c")
	cSubtext0 = lipgloss.Color("#a6adc8")
	cSubtext1 = lipgloss.Color("#bac2de")
	cText     = lipgloss.Color("#cdd6f4")
	cBlue     = lipgloss.Color("#89b4fa")
	cLavender = lipgloss.Color("#b4befe")
	cSapphire = lipgloss.Color("#74c7ec")
	cTeal     = lipgloss.Color("#94e2d5")
	cGreen    = lipgloss.Color("#a6e3a1")
	cYellow   = lipgloss.Color("#f9e2af")
	cPeach    = lipgloss.Color("#fab387")
	cRed      = lipgloss.Color("#f38ba8")
	cMauve    = lipgloss.Color("#cba6f7")
	cPink     = lipgloss.Color("#f5c2e7")
)

var (
	userStyle      = lipgloss.NewStyle().Bold(true).Foreground(cBlue).Background(cBase)
	assistantStyle = lipgloss.NewStyle().Foreground(cText).Background(cCard)
	toolStyle      = lipgloss.NewStyle().Foreground(cGreen).Background(cBase)
	resultStyle    = lipgloss.NewStyle().Foreground(cSubtext0).Background(cBase)
	errStyle       = lipgloss.NewStyle().Foreground(cRed).Background(cBase)
	noticeStyle    = lipgloss.NewStyle().Foreground(cYellow).Background(cBase)
	skillLineStyle = lipgloss.NewStyle().Foreground(cPink).Background(cBase).Bold(true)
	dlHeadStyle    = lipgloss.NewStyle().Foreground(cBlue).Background(cBase).Bold(true)

	// screenStyle paints the whole frame so no terminal background shows through.
	screenStyle = lipgloss.NewStyle().Background(cBase)
	// statusBarStyle is the bottom bar; it shares the mantle canvas and is set off
	// by its colored segment text rather than a distinct background.
	statusBarStyle = lipgloss.NewStyle().Foreground(cSubtext0).Background(cMantle).Padding(0, 1)

	// gutterStyle gives a finalized assistant answer a colored left bar.
	gutterStyle = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder(), false, false, false, true).
			BorderForeground(cMauve).
			BorderBackground(cBase).
			Background(cCard).
			PaddingLeft(2)

	// liveGutterStyle marks the still-streaming answer with a muted bar.
	liveGutterStyle = gutterStyle.BorderForeground(cSurface2)

	// inputBoxStyle frames the input area with a rounded accent border.
	inputBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(cLavender).
			BorderBackground(cBase).
			Background(cBase).
			Padding(0, 1)

	// overlayBorder hugs the panel with half-block glyphs: the inner half of each
	// edge carries the border color while the outer half stays the chat
	// background. A thin centered line shares one background across the whole
	// cell, so it always mismatches one side (a dark seam toward the panel or a
	// light halo toward the chat); splitting the cell removes both.
	overlayBorder = lipgloss.Border{
		Top:         "▄",
		Bottom:      "▀",
		Left:        "▐",
		Right:       "▌",
		TopLeft:     "▗",
		TopRight:    "▖",
		BottomLeft:  "▝",
		BottomRight: "▘",
	}

	// overlayBoxStyle frames the confirm prompt and resume picker.
	overlayBoxStyle = lipgloss.NewStyle().
			Border(overlayBorder).
			BorderForeground(cMauve).
			BorderBackground(cBase).
			Background(cSurface0).
			Padding(0, 1)

	overlayTitleStyle = lipgloss.NewStyle().Foreground(cSubtext1).Background(cSurface0).Bold(true)
	overlayTextStyle  = lipgloss.NewStyle().Foreground(cSubtext0).Background(cSurface0)
	overlayHintStyle  = lipgloss.NewStyle().Foreground(cOverlay1).Background(cSurface0)
	overlayWarnStyle  = lipgloss.NewStyle().Foreground(cRed).Background(cSurface0).Bold(true)

	optSelStyle  = lipgloss.NewStyle().Bold(true).Foreground(cCrust).Background(cGreen).Padding(0, 1)
	optStyle     = lipgloss.NewStyle().Foreground(cSubtext1).Background(cSurface1).Padding(0, 1)
	pickSelStyle = lipgloss.NewStyle().Foreground(cCrust).Background(cBlue)
	pickRowStyle = lipgloss.NewStyle().Foreground(cSubtext1).Background(cSurface0)
)

// ---- messages bridged from the agent goroutine ----

type contentMsg string

// assistantStepMsg carries a completed intermediate assistant message: text the
// loop will continue past (tool calls or an evaluator/Stop-hook push). The UI
// commits it to the timeline and resets the live preview.
type assistantStepMsg string
type reasoningMsg string
type usageMsg int
type todoUpdateMsg []agent.TodoItem
type toolStartMsg struct {
	name string
	args string
}
type toolEndMsg struct {
	name, result string
	err          error
}

// fileChangeMsg reports a file written or edited by a tool, for session-artifact
// tracking. Paths are absolute; old/new are the file's content around the change.
type fileChangeMsg struct {
	path, old, new string
	created        bool
}
type agentStartMsg struct {
	id, agent, prompt string
}
type agentEndMsg struct {
	id, result string
	err        error
}
type subToolStartMsg struct {
	id, agent, name, args string
}
type subToolEndMsg struct {
	id, agent, name, result string
	err                     error
}
type subNoticeMsg struct {
	id, agent, text string
}

// confirmReqMsg is a request for the confirmation overlay. It covers two kinds
// of question - "run this tool?" and "reach outside the working directory?" -
// because they share the overlay, the one-at-a-time rule, and the queue behind
// it. pathResp being non-nil marks the second kind; resp marks the first.
type confirmReqMsg struct {
	name, args string
	resp       chan bool

	path     string // absolute path outside the root, for a path request
	write    bool   // the tool wants to modify it, not just read it
	pathResp chan tools.PathDecision
}
type turnDoneMsg struct {
	answer string
	err    error
}
type noticeMsg string

// blockKind tags an entry in the persisted conversation so it can be
// re-rendered (e.g. to show/hide tool output).
type blockKind int

const (
	bkUser blockKind = iota
	bkToolCall
	bkToolResult
	bkToolError
	bkAssistant
	bkNotice
	bkError
	bkAgentGroup // placeholder in the timeline for a (possibly running) subagent
	bkSkill      // a user-invoked skill line (/skill:name)
	bkMcpPrompt  // a user-invoked MCP prompt line (/mcp__server__prompt)
	bkDiff       // a file's side-by-side diff, opened from /artifacts
	bkDownload   // live local-model download status (model name + progress bar)
)

type block struct {
	kind    blockKind
	text    string
	depth   int      // 0 = main agent, 1 = subagent (rendered indented)
	ref     string   // for bkAgentGroup: the agent run id (key into Model.groups)
	diffOps []diffOp // for bkDiff: the precomputed line edits, laid out per frame
}

// agentRun accumulates one subagent delegation's activity so concurrent runs
// render as self-contained, correctly-attributed groups regardless of the
// order their events interleave.
type agentRun struct {
	id     string
	name   string
	prompt string
	lines  []block
	done   bool
	failed bool
}

// confirm choices, shown as a small box above the input.
type confirmChoice int

const (
	choiceOnce confirmChoice = iota
	choiceAlways
	choiceForbid
)

// resumePicker holds the session list shown by /resume.
type resumePicker struct {
	items  []session.Meta
	cursor int
}

// skillBrowser is the scrollable skill list shown by /skills, with a per-skill
// detail view.
type skillBrowser struct {
	items  []*skill.Skill
	cursor int
	detail bool
}

// mcpResItem is one selectable resource in the MCP browser.
type mcpResItem struct {
	server string
	uri    string
	name   string
}

// mcpBrowser is the read-only /mcp view: server status plus a selectable list of
// resources that can be previewed.
type mcpBrowser struct {
	servers []mcp.ServerView
	items   []mcpResItem // flattened resources across servers, selectable
	cursor  int
	preview string // resource text when previewing
	loading bool
}

// mcpPreviewMsg carries an async resource read for the browser preview pane.
type mcpPreviewMsg struct {
	text string
	err  error
}

// commandItem is one entry in the slash-command autocomplete menu.
type commandItem struct {
	name string
	desc string
}

// commandMenu is the live fuzzy-filtered list shown while the user types a
// slash command in the input.
type commandMenu struct {
	items  []commandItem
	cursor int
}

// modelItem is one selectable model in the /model picker.
type modelItem struct {
	ref      string // provider/id
	name     string
	provider string
	locked   bool // provider needs auth and is not authenticated
	current  bool
}

// modelPicker is the fuzzy-filtered model list shown by /model.
type modelPicker struct {
	all    []modelItem // every model, unfiltered
	items  []modelItem // current filtered view
	cursor int
	query  string
}

// localModelChoice is the action widget shown after selecting a local model.
type localModelChoice struct {
	ref string
	idx int // 0 Use, 1 Configure, 2 Drop
}

// loginDoneMsg reports the result of a background /login flow. thenSwitch, when
// set, is the model ref to switch to on success (a login started from a locked
// /model entry).
type loginDoneMsg struct {
	provider   string
	thenSwitch string
	err        error
}

// localStartedMsg reports the result of a background local-server start. cfg is
// the config that was started, used to switch to the local model on success.
type localStartedMsg struct {
	cfg local.Config
	err error
}

// localProgressMsg streams llama-server download/load progress to the TUI.
type localProgressMsg local.Progress

// availabilityMsg carries the result of the startup check of the active model;
// alert is nil when the model is available.
type availabilityMsg struct{ alert *alertBox }

// alertAction is what an alertBox's confirm button does.
type alertAction int

const (
	alertDismiss    alertAction = iota
	alertLocalSetup             // open the local-model setup wizard
	alertLocalStart             // download/start the configured local model
	alertLocalDrop              // delete the configured local model and its files
	alertLogin                  // log in to a provider
)

// alertBox is a dismissable warning shown when the active model is not available
// (local not set up / not running, or a provider not authenticated).
type alertBox struct {
	title        string
	body         string
	confirmLabel string // "" hides the confirm action
	action       alertAction
	cfg          local.Config // for alertLocalStart
	provider     string       // for alertLogin
}

type Model struct {
	agent      *agent.Agent
	backend    *llm.Ref
	modelReg   *llm.Registry
	compactCfg agent.CompactConfig
	model      string
	url        string
	events     chan tea.Msg
	done       chan struct{} // closed on quit to unblock the agent goroutine
	cancel     context.CancelFunc

	vp             viewport.Model
	input          textarea.Model
	spin           spinner.Model
	md             *glamour.TermRenderer // prose renderer (card background)
	mdCode         *glamour.TermRenderer // fenced-code renderer (surface0 panel)
	width          int
	height         int
	ready          bool
	busy           bool
	showToolOutput bool
	pendingImages  []llm.Image
	pastingImage   bool

	pending        *confirmReqMsg
	pendingQueue   []*confirmReqMsg // confirmations waiting behind the current one
	confirmIdx     confirmChoice
	autoMode       bool                 // shift+tab: auto-approve all but irreversible (destructive) ops
	toolPolicy     map[string]string    // tool name -> "allow" | "deny" for the session
	groups         map[string]*agentRun // subagent runs by parent-call id
	skills         *skill.Registry
	agents         *agent.SubagentRegistry
	reg            *tools.Registry
	searchPrompted bool // web_search was advertised in the system prompt
	mcpMgr         *mcp.Manager
	hooks          *hooks.Runner
	trustAsk       bool                  // project-local hooks await a trust decision
	skillAsk       *skill.PendingSkills  // project-local skills await a trust decision
	skillAskCwd    string                // cwd the pending skills were resolved from
	regSkillTool   func(*skill.Registry) // (re-)registers the skill tool once skills are approved
	picker         *resumePicker
	browser        *skillBrowser
	agentBr        *agentBrowser
	mcp            *mcpBrowser
	models         *modelPicker
	localChoice    *localModelChoice // local-model action widget, nil when closed
	localWiz       *localWizard      // /model init overlay, nil when closed
	localProgIdx   int               // index of the live local-start progress block, -1 when none
	dlName         string            // model name shown in the active download block
	prog           progress.Model    // progress bar for local downloads
	alert          *alertBox         // active-model-unavailable warning, nil when none
	cmdMenu        *commandMenu
	commands       []commandItem
	fileMenu       *fileMenu  // @-path autocomplete, nil when closed
	files          []fileItem // lazily-built project file index
	filePaths      []string   // files' paths, cached for per-keystroke fuzzy matching
	filesIndexed   bool

	artBr          *artifactBrowser     // /artifacts picker, nil when closed
	artifacts      []*artifact          // files changed this session, in first-touch order
	artIndex       map[string]*artifact // rel path -> artifact, to merge repeat edits
	diffScrollX    int                  // horizontal scroll offset shared by all diff blocks
	diffMaxLen     int                  // longest diff content line, to bound horizontal scroll
	ctxSize        int
	defaultCtxSize int // fallback gauge window for models with no ContextWindow
	ctxTokens      int
	maxTokens      int
	sessionID      string
	sessionTitle   string
	sessionStart   time.Time

	history   []string // submitted prompts, for up/down recall
	histIdx   int      // == len(history) means "current draft"
	histDraft string

	blocks        []block          // persisted conversation
	todos         []agent.TodoItem // model's working plan, shown in the right sidebar
	todoCollapsed bool             // plan panel collapsed to a single header line
	curContent    string           // streaming answer this turn (full raw markdown)
	curReason     string           // streaming reasoning this turn (collapsed behind spinner)

	// Progressive markdown: curContent[:curStableLen] holds complete blocks
	// already rendered into curStableRender; the rest is the still-streaming
	// tail shown as plain text until its block closes.
	curStableLen    int
	curStableRender string

	// Mouse text selection. Points are content coordinates (absolute viewport
	// line + visual column) so they survive scrolling. vpContent caches the raw
	// styled content set on the viewport, for extracting the selected text.
	vpContent string
	selecting bool     // a left-drag is in progress
	selActive bool     // a non-empty selection is highlighted
	selAnchor selPoint // where the drag began
	selHead   selPoint // where the cursor is now

	// rebuildSystem re-assembles the system prompt from the current on-disk
	// project instructions (AGENTS.md/CLAUDE.md/context.md) so /new picks up edits
	// made since launch. nil leaves the launch-time prompt in place.
	rebuildSystem func() string
}

// selPoint is a position in the viewport content: an absolute line index and a
// visual column (cell) offset.
type selPoint struct{ line, col int }

// SetSystemRebuilder registers a closure that rebuilds the system prompt from
// the current on-disk project instructions; /new calls it so edits to
// AGENTS.md/CLAUDE.md/context.md take effect without a full restart.
func (m *Model) SetSystemRebuilder(f func() string) { m.rebuildSystem = f }

// SetStartupNotices seeds the chat with warnings raised before the program
// started. They are also on stderr, but the alt screen wipes that on the first
// frame, so anything the user has to act on has to be repeated here.
func (m *Model) SetStartupNotices(notices []string) {
	for _, n := range notices {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: n})
	}
}

// SetPendingSkills arms the startup prompt for project-local skills that
// discovery withheld for lack of approval; a nil value disarms it. cwd must be
// the one Pending resolved them from, since approving resolves the project root
// again and a different cwd can land on a different root. Call it before the
// first keystroke reaches the model - it has no effect once a turn is under way.
func (m *Model) SetPendingSkills(cwd string, p *skill.PendingSkills) {
	m.skillAskCwd, m.skillAsk = cwd, p
}

func New(client *llm.Ref, reg *tools.Registry, temp float64, modelName, url, systemPrompt string,
	ctxSize, maxTokens int, agents *agent.SubagentRegistry, project string, skills *skill.Registry,
	runner *hooks.Runner, mcpMgr *mcp.Manager, sessionTitle string, compactCfg agent.CompactConfig,
	modelReg *llm.Registry) Model {
	// Buffered generously: parallel subagents can burst many events at once.
	events := make(chan tea.Msg, 256)
	done := make(chan struct{})

	confirm := func(name string, args json.RawMessage) bool {
		resp := make(chan bool, 1)
		select {
		case events <- confirmReqMsg{name: name, args: string(args), resp: resp}:
		case <-done:
			return false
		}
		select {
		case ok := <-resp:
			return ok
		case <-done:
			return false
		}
	}
	// A path outside the working directory asks through the same overlay as a
	// tool confirmation, so the two cannot appear at once. Persisted grants are
	// enabled here and nowhere else: an unattended bot must not inherit a
	// directory a human approved for the same working directory.
	reg.SetPathGrants(true)
	reg.SetPathApprover(func(path string, intent tools.PathIntent) tools.PathDecision {
		resp := make(chan tools.PathDecision, 1)
		select {
		case events <- confirmReqMsg{name: intent.Tool, path: path, write: intent.Write, pathResp: resp}:
		case <-done:
			return tools.PathDeny
		}
		select {
		case d := <-resp:
			return d
		case <-done:
			return tools.PathDeny
		}
	})

	// Ride out transient provider failures (429/5xx, an overloaded backend, a
	// dropped stream) instead of surfacing them into the session. It wraps the
	// Ref rather than the backend inside it, so a live /model switch keeps the
	// retries. A stream that already emitted text is not retried, so an
	// interruption mid-answer still surfaces - the deltas were delivered and a
	// second attempt would duplicate them.
	stream := llm.NewRetrying(client, retryAttempts)
	stream.SetOnRetry(func(n llm.RetryNotice) {
		select {
		case events <- noticeMsg(formatRetry(n)):
		case <-done:
		}
	})
	if agents != nil {
		reg.Register(agent.NewTaskTool(stream, reg, temp, confirm, agents, project))
	}
	// Kept as a closure: a project whose only skills are untrusted has none to
	// advertise at launch, so the tool is registered again once they are approved.
	// It takes the registry rather than closing over one, so the caller cannot
	// silently register a tool against a registry it has since replaced.
	regSkillTool := func(sk *skill.Registry) {
		if st := agent.NewSkillTool(sk, stream, reg, temp, confirm); st != nil {
			reg.Register(st)
		}
	}
	regSkillTool(skills)
	reg.OnFileChange(func(c tools.FileChange) {
		select {
		case events <- fileChangeMsg{path: c.Path, old: c.Old, new: c.New, created: c.Created}:
		case <-done:
		}
	})
	ag := agent.New(stream, reg, temp, confirm, systemPrompt)
	reg.Register(agent.NewTodoTool(ag))
	ag.SetHooks(runner)
	ag.SetCompaction(compactCfg)
	if skills != nil {
		ag.WatchSkills(skills.Conditional())
	}

	ta := textarea.New()
	ta.Placeholder = "Ask aigem...  (Enter send · / for commands · Ctrl+C quit)"
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.MaxHeight = maxInputHeight
	ta.SetHeight(1)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle().Background(cBase)
	ta.FocusedStyle.Base = lipgloss.NewStyle().Background(cBase)
	ta.FocusedStyle.Text = lipgloss.NewStyle().Foreground(cText).Background(cBase)
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(cOverlay0).Background(cBase)
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(cBlue).Background(cBase).Bold(true)
	// Mirror the focused look onto the blurred state so that blurring the input
	// (done while an overlay is open) only drops the blinking cursor, without
	// recoloring the box to the gray default blurred theme.
	ta.BlurredStyle = ta.FocusedStyle
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(cMauve).Background(cBase)

	if ctxSize <= 0 {
		ctxSize = 8192
	}
	_, searchOn := reg.Get("web_search")
	// Availability of the active model is checked asynchronously after launch (see
	// Init/checkActiveAvailability), so a reachability probe never delays startup.
	return Model{
		localProgIdx:   -1,
		prog:           progress.New(progress.WithDefaultGradient()),
		agent:          ag,
		backend:        client,
		modelReg:       modelReg,
		compactCfg:     compactCfg,
		model:          modelName,
		url:            url,
		events:         events,
		done:           done,
		input:          ta,
		spin:           sp,
		toolPolicy:     map[string]string{},
		groups:         map[string]*agentRun{},
		artIndex:       map[string]*artifact{},
		skills:         skills,
		agents:         agents,
		reg:            reg,
		searchPrompted: searchOn,
		mcpMgr:         mcpMgr,
		commands:       buildCommands(skills, mcpMgr),
		hooks:          runner,
		trustAsk:       runner != nil && runner.HasUntrustedProjectHooks(),
		regSkillTool:   regSkillTool,
		sessionTitle:   sessionTitle,
		ctxSize:        ctxSize,
		defaultCtxSize: ctxSize,
		maxTokens:      maxTokens,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.spin.Tick, m.waitForEvent(), m.checkActiveAvailability())
}

// checkActiveAvailability verifies the startup model is actually usable, off the
// render path (the local probe can take a couple of seconds), surfacing an
// availabilityMsg whose alert is non-nil when the model is unavailable.
func (m Model) checkActiveAvailability() tea.Cmd {
	ref := m.model
	return func() tea.Msg { return availabilityMsg{alert: assessActiveModel(ref)} }
}

// assessActiveModel returns a warning for an unavailable active model, or nil
// when it is available. Local availability means the server is reachable;
// provider availability means it is authenticated.
func assessActiveModel(ref string) *alertBox {
	prov, _, _ := strings.Cut(ref, "/")
	if prov == llm.LocalProviderID {
		cfg, exists, _ := local.Load()
		switch local.Assess(cfg, exists) {
		case local.ActionNeedsInit:
			return &alertBox{
				title: "Local model not set up", action: alertLocalSetup,
				body:         cfg.ModelName + " has not been downloaded yet.",
				confirmLabel: "Set up & download",
			}
		case local.ActionNeedsStart:
			return &alertBox{
				title: "Local model unavailable", action: alertLocalStart, cfg: cfg,
				body:         cfg.ModelName + " is not running. Download and start it now?",
				confirmLabel: "Download & start",
			}
		}
		return nil
	}
	if prov != "" && !auth.IsAuthenticated(prov) {
		a := &alertBox{title: prov + " not authenticated", provider: prov}
		if prov == llm.OpenAIProviderID {
			// Only the OpenAI provider has an interactive (browser) login flow.
			a.body = "The active model needs a login before it can be used."
			a.action, a.confirmLabel = alertLogin, "Log in"
		} else {
			a.body = "The active model's provider is not authenticated. Run: aigem auth login " + prov
		}
		return a
	}
	return nil
}

// waitForEvent blocks on the bridge channel and surfaces the next agent message.
func (m Model) waitForEvent() tea.Cmd {
	return func() tea.Msg { return <-m.events }
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.layout()

	case tea.KeyMsg:
		if cmd, handled := m.handleKey(msg); handled {
			return m, tea.Batch(cmd, m.reconcileFocus())
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)
		if m.busy {
			m.refresh()
		}

	case contentMsg:
		m.curContent += string(msg)
		m.updateStable()
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case assistantStepMsg:
		// Commit the finished step's text to the timeline and clear the live
		// preview so it never lingers beneath the tool output that follows.
		if strings.TrimSpace(string(msg)) != "" {
			m.blocks = append(m.blocks, block{kind: bkAssistant, text: string(msg)})
		}
		m.resetStream()
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case reasoningMsg:
		// Reasoning is collapsed behind the spinner; the tick loop animates it.
		m.curReason += string(msg)
		cmds = append(cmds, m.waitForEvent())

	case usageMsg:
		m.ctxTokens = int(msg)
		cmds = append(cmds, m.waitForEvent())

	case todoUpdateMsg:
		// The plan panel floats over the chat (full-width), so a plan change needs
		// no relayout - View() composites it from m.todos each frame.
		m.todos = []agent.TodoItem(msg)
		cmds = append(cmds, m.waitForEvent())

	case toolStartMsg:
		m.blocks = append(m.blocks, block{kind: bkToolCall, text: fmt.Sprintf("%s › %s", msg.name, formatArgs(msg.name, msg.args))})
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case toolEndMsg:
		if msg.err != nil {
			m.blocks = append(m.blocks, block{kind: bkToolError, text: msg.err.Error()})
		} else {
			m.blocks = append(m.blocks, block{kind: bkToolResult, text: firstLines(msg.result, 8)})
		}
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case fileChangeMsg:
		m.recordArtifact(msg)
		m.refresh() // updates the status-bar artifact count
		cmds = append(cmds, m.waitForEvent())

	case agentStartMsg:
		m.groups[msg.id] = &agentRun{id: msg.id, name: msg.agent, prompt: msg.prompt}
		m.blocks = append(m.blocks, block{kind: bkAgentGroup, ref: msg.id})
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case agentEndMsg:
		if g := m.groups[msg.id]; g != nil {
			g.done = true
			g.failed = msg.err != nil
		}
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case subToolStartMsg:
		m.appendToGroup(msg.id, block{kind: bkToolCall, depth: 1,
			text: fmt.Sprintf("%s › %s", msg.name, formatArgs(msg.name, msg.args))})
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case subToolEndMsg:
		if msg.err != nil {
			m.appendToGroup(msg.id, block{kind: bkToolError, depth: 1, text: msg.err.Error()})
		} else {
			m.appendToGroup(msg.id, block{kind: bkToolResult, depth: 1, text: firstLines(msg.result, 8)})
		}
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case subNoticeMsg:
		m.appendToGroup(msg.id, block{kind: bkNotice, depth: 1, text: msg.text})
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case confirmReqMsg:
		m.handleConfirmReq(msg)
		cmds = append(cmds, m.waitForEvent())

	case noticeMsg:
		m.blocks = append(m.blocks, block{kind: bkNotice, text: string(msg)})
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case imagePasteMsg:
		m.pastingImage = false
		if msg.err != nil {
			m.blocks = append(m.blocks, block{kind: bkNotice, text: msg.err.Error()})
		} else {
			m.pendingImages = append(m.pendingImages, msg.image)
			m.insertImageMarker(len(m.pendingImages))
		}
		m.refresh()

	case mcpPreviewMsg:
		// Ignore a read that completes after the user already left the preview
		// (Esc clears loading), so it does not pop the dismissed pane back open.
		if m.mcp != nil && m.mcp.loading {
			m.mcp.loading = false
			if msg.err != nil {
				m.mcp.preview = "error: " + msg.err.Error()
			} else if strings.TrimSpace(msg.text) == "" {
				m.mcp.preview = "(empty resource)"
			} else {
				m.mcp.preview = msg.text
			}
			m.layout()
		}

	case agentCfgDoneMsg:
		// Delivered by the saveSearchCfg tea.Cmd, not the event bridge.
		m.applyAgentCfg(msg)

	case tea.MouseMsg:
		cmds = append(cmds, m.handleMouse(msg))

	case turnDoneMsg:
		m.finishTurn(msg)
		cmds = append(cmds, m.waitForEvent())

	case loginDoneMsg:
		// Delivered by the runLogin tea.Cmd, not the event bridge, so no rewait.
		m.busy = false
		if msg.err != nil {
			m.blocks = append(m.blocks, block{kind: bkError, text: "login failed: " + msg.err.Error()})
		} else {
			m.blocks = append(m.blocks, block{kind: bkNotice, text: "logged in to " + msg.provider})
			if msg.thenSwitch != "" {
				m.switchModel(msg.thenSwitch, true)
			}
		}
		m.refresh()

	case availabilityMsg:
		// Don't steal focus from an overlay the user already opened.
		if msg.alert != nil && m.alert == nil && !m.anyOverlayOpen() {
			m.alert = msg.alert
			m.layout()
		}

	case localProgressMsg:
		if m.localProgIdx >= 0 && m.localProgIdx < len(m.blocks) {
			m.prog.Width = max(20, min(48, m.width-30))
			m.blocks[m.localProgIdx].text = m.downloadBlockText(local.Progress(msg))
		}
		m.refresh()
		cmds = append(cmds, m.waitForEvent())

	case localStartedMsg:
		m.busy = false
		if msg.err != nil {
			m.localProgIdx = -1
			m.dlName = ""
			m.blocks = append(m.blocks, block{kind: bkError, text: "llama-server: " + msg.err.Error()})
			m.refresh()
			return m, nil
		}
		// The server is up, so the download and load are complete: pin the block to
		// 100% deterministically. The final PhaseReady frame is unreliable - a cache
		// hit reaches "ready" before it arrives, and this message can outrace it.
		if m.localProgIdx >= 0 && m.localProgIdx < len(m.blocks) {
			m.blocks[m.localProgIdx].text = m.downloadDoneText()
		}
		m.localProgIdx = -1
		m.dlName = ""
		m.modelReg.ReplaceLocal(llm.LocalProvider(msg.cfg.BaseURL(), msg.cfg.ModelName, msg.cfg.CtxSize, m.maxTokens))
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "llama-server is up"})
		m.switchModel(llm.LocalProviderID+"/"+msg.cfg.ModelName, true)
		return m, nil
	}

	// Keep the input interactive. Keys are NOT forwarded to the viewport: its
	// default keymap binds plain letters (j/k/d/u/f/b/space) which would scroll
	// while typing - scrolling is handled explicitly in handleKey and via mouse.
	_, isKey := msg.(tea.KeyMsg)

	// Size the textarea to its full height before it handles the key. The
	// textarea scrolls its own viewport to keep the cursor visible; against a
	// transiently-too-short height (the box only grows a row after the key is
	// processed) it would scroll the first line out of view and never scroll
	// back. Growing first, then shrinking to the real content height, keeps the
	// box top-aligned whenever the text fits. Only the input content (keys)
	// changes the height, so non-key messages (blinks, ticks) skip the dance.
	prevH := m.input.Height()
	if isKey {
		m.input.SetHeight(maxInputHeight)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	cmds = append(cmds, cmd)

	if isKey {
		rows := inputRows(m.input.Value(), m.input.Width())
		newH := min(maxInputHeight, rows)
		m.input.SetHeight(newH)

		// Deleting back down from an overflow leaves the textarea's viewport
		// scrolled even though everything now fits, clipping the first line under
		// the border. The textarea only ever scrolls down to chase the cursor,
		// never back up, so re-pin to the top by round-tripping the value (this
		// also rewinds the viewport). Limited to the at-cap case so ordinary
		// typing never pays for it.
		if rows <= maxInputHeight && prevH == maxInputHeight && cursorAtInputEnd(m.input) {
			m.input.SetValue(m.input.Value())
			m.input.CursorEnd()
		}

		if newH != prevH {
			m.layout()
		}

		m.syncCommandMenu()
		m.syncFileMenu()
		m.clearSelection() // typing or navigating drops a stale highlight
	}

	cmds = append(cmds, m.reconcileFocus())
	return m, tea.Batch(cmds...)
}

// handleMouse routes mouse events: the wheel scrolls the viewport, a click on the
// plan panel toggles its collapse, and a left-drag selects text in the chat and
// auto-copies it to the clipboard on release.
func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return cmd
	}
	if msg.Button != tea.MouseButtonLeft {
		return nil
	}
	if msg.Action == tea.MouseActionPress &&
		m.sidebarVisible() && msg.X >= m.width-sidebarWidth && msg.Y < len(m.sidebarLines()) {
		m.todoCollapsed = !m.todoCollapsed
		m.selecting, m.selActive = false, false
		return nil
	}
	// Selection state lives only in the View; it changes no content, so it needs
	// no refresh - Bubble Tea re-renders the View after every event.
	switch msg.Action {
	case tea.MouseActionPress:
		if p, ok := m.pointAt(msg.X, msg.Y); ok {
			m.selecting, m.selActive = true, false
			m.selAnchor, m.selHead = p, p
		}
	case tea.MouseActionMotion:
		if m.selecting {
			if p, ok := m.pointAt(msg.X, msg.Y); ok {
				m.selHead = p
				m.selActive = m.selAnchor != m.selHead
			}
		}
	case tea.MouseActionRelease:
		if m.selecting {
			m.selecting = false
			if m.selActive {
				if text := m.selectedText(); text != "" {
					return copyToClipboard(text)
				}
			}
			m.selActive = false
		}
	}
	return nil
}

// pointAt maps a screen cell to a content coordinate, or reports false when the
// cell is outside the chat viewport.
func (m Model) pointAt(x, y int) (selPoint, bool) {
	if x < 0 || y < 0 || y >= m.vp.Height {
		return selPoint{}, false
	}
	return selPoint{line: m.vp.YOffset + y, col: x}, true
}

// clearSelection drops any in-progress or highlighted selection. The View picks
// the change up on the next render, so no refresh is needed.
func (m *Model) clearSelection() {
	m.selecting, m.selActive = false, false
}

// selBounds returns the selection endpoints ordered top-left to bottom-right.
func (m Model) selBounds() (selPoint, selPoint) {
	a, b := m.selAnchor, m.selHead
	if a.line > b.line || (a.line == b.line && a.col > b.col) {
		a, b = b, a
	}
	return a, b
}

// selectedText extracts the plain text covered by the current selection from the
// cached viewport content, trimming each line's trailing fill.
func (m Model) selectedText() string {
	a, b := m.selBounds()
	lines := strings.Split(m.vpContent, "\n")
	var out []string
	for ln := a.line; ln <= b.line && ln < len(lines); ln++ {
		plain := ansiRE.ReplaceAllString(lines[ln], "")
		from := 0
		if ln == a.line {
			from = a.col
		}
		to := ansi.StringWidth(plain)
		if ln == b.line {
			to = min(b.col, to)
		}
		if to <= from {
			out = append(out, "")
			continue
		}
		out = append(out, strings.TrimRight(ansi.Cut(plain, from, to), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// copyToClipboard writes text to the terminal clipboard via OSC 52, which tmux
// and most terminals forward to the system clipboard.
func copyToClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		termenv.Copy(text)
		return nil
	}
}

// highlightSelection reverse-videos the selected cells on each visible chat line.
// Reverse is a character attribute, not a background color, so it survives the
// renderer that drops painted backgrounds. The selected run is flattened to plain
// text so the attribute applies evenly; the unselected sides keep their styling.
func (m Model) highlightSelection(chat string) string {
	a, b := m.selBounds()
	lines := strings.Split(chat, "\n")
	for y := range lines {
		cl := m.vp.YOffset + y
		if cl < a.line || cl > b.line {
			continue
		}
		w := ansi.StringWidth(lines[y])
		from := 0
		if cl == a.line {
			from = a.col
		}
		to := w
		if cl == b.line {
			to = min(b.col, w)
		}
		if from >= w || to <= from {
			continue
		}
		left := ansi.Cut(lines[y], 0, from)
		mid := ansiRE.ReplaceAllString(ansi.Cut(lines[y], from, to), "")
		right := ansi.Cut(lines[y], to, w)
		lines[y] = left + "\x1b[0m\x1b[7m" + mid + "\x1b[27m\x1b[0m" + right
	}
	return strings.Join(lines, "\n")
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if m.trustAsk {
		return m.handleTrustKey(msg), true
	}
	if m.skillAsk != nil {
		return m.handleSkillTrustKey(msg), true
	}
	if m.pending != nil {
		return m.handleConfirmKey(msg), true
	}
	if m.alert != nil {
		return m.handleAlertKey(msg), true
	}
	if m.picker != nil {
		return m.handlePickerKey(msg), true
	}
	if m.browser != nil {
		return m.handleSkillKey(msg), true
	}
	if m.agentBr != nil {
		return m.handleAgentKey(msg), true
	}
	if m.mcp != nil {
		return m.handleMcpKey(msg), true
	}
	if m.localWiz != nil {
		return m.handleLocalWizardKey(msg), true
	}
	if m.localChoice != nil {
		return m.handleLocalChoiceKey(msg), true
	}
	if m.models != nil {
		return m.handleModelKey(msg), true
	}
	if m.artBr != nil {
		return m.handleArtifactKey(msg), true
	}
	if m.cmdMenu != nil {
		if cmd, handled := m.handleCmdMenuKey(msg); handled {
			return cmd, true
		}
	}
	if m.fileMenu != nil {
		if cmd, handled := m.handleFileMenuKey(msg); handled {
			return cmd, true
		}
	}

	switch s := msg.String(); s {
	case "shift+tab":
		m.autoMode = !m.autoMode
		text := "auto mode OFF - every edit and command asks for confirmation"
		if m.autoMode {
			text = "auto mode ON - approving edits and safe commands automatically; " +
				"irreversible deletions still ask"
		}
		m.blocks = append(m.blocks, block{kind: bkNotice, text: text})
		m.refresh()
		return nil, true
	case "shift+enter", "alt+enter":
		return nil, false // let the textarea insert a newline
	case "ctrl+v":
		if m.busy || m.pastingImage {
			return nil, true
		}
		m.pastingImage = true
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "reading image from clipboard..."})
		m.refresh()
		return pasteImageFromClipboardCmd(), true
	case "shift+up":
		m.vp.ScrollUp(2)
		return nil, true
	case "shift+down":
		m.vp.ScrollDown(2)
		return nil, true
	case "shift+left":
		m.scrollDiff(-diffScrollStep)
		return nil, true
	case "shift+right":
		m.scrollDiff(diffScrollStep)
		return nil, true
	}

	switch msg.Type {
	case tea.KeyCtrlC:
		if m.cancel != nil {
			m.cancel()
		}
		if !m.busy {
			m.saveSession()
		}
		if m.hooks != nil {
			m.hooks.RunBounded(hooks.EventSessionEnd, hooks.Input{Source: "exit"}, 5*time.Second)
		}
		select {
		case <-m.done:
		default:
			close(m.done)
		}
		return tea.Quit, true
	case tea.KeyEsc:
		if m.busy && m.cancel != nil {
			m.cancel()
			return nil, true
		}
		if len(m.pendingImages) > 0 {
			m.pendingImages = nil
			m.input.SetValue(stripImageMarkers(m.input.Value()))
			m.input.CursorEnd()
			m.blocks = append(m.blocks, block{kind: bkNotice, text: "cleared attached images"})
			m.refresh()
		}
		return nil, true
	case tea.KeyCtrlO:
		m.showToolOutput = !m.showToolOutput
		m.refresh()
		return nil, true
	case tea.KeyCtrlT:
		if len(m.todos) > 0 {
			m.todoCollapsed = !m.todoCollapsed
		}
		return nil, true
	case tea.KeyPgUp:
		m.vp.PageUp()
		return nil, true
	case tea.KeyPgDown:
		m.vp.PageDown()
		return nil, true
	case tea.KeyUp:
		if !strings.Contains(m.input.Value(), "\n") {
			m.navHistory(-1)
			return nil, true
		}
	case tea.KeyDown:
		if !strings.Contains(m.input.Value(), "\n") {
			m.navHistory(1)
			return nil, true
		}
	case tea.KeyEnter:
		if m.busy {
			return nil, true
		}
		input := strings.TrimSpace(m.input.Value())
		if input == "" && len(m.pendingImages) == 0 {
			return nil, true
		}
		return m.dispatch(input), true
	}
	return nil, false
}

// dispatch routes a submitted line to a slash command or the agent.
func (m *Model) dispatch(input string) tea.Cmd {
	m.history = append(m.history, input)
	m.histIdx = len(m.history)
	switch {
	case input == "/new":
		m.newSession()
		return nil
	case input == "/resume":
		m.input.Reset()
		m.openPicker()
		return nil
	case input == "/skills":
		m.input.Reset()
		m.openSkills()
		return nil
	case input == "/agents":
		m.input.Reset()
		m.openAgents()
		return nil
	case input == "/artifacts":
		m.input.Reset()
		m.openArtifacts()
		return nil
	case strings.HasPrefix(input, "/skill:"):
		m.input.Reset()
		return m.runSkill(strings.TrimPrefix(input, "/skill:"))
	case input == "/mcp":
		m.input.Reset()
		m.openMcp()
		return nil
	case strings.HasPrefix(input, "/mcp__"):
		m.input.Reset()
		return m.runMcpPrompt(strings.TrimPrefix(input, "/"))
	case input == "/compact" || strings.HasPrefix(input, "/compact "):
		m.input.Reset()
		return m.runCompact(strings.TrimSpace(strings.TrimPrefix(input, "/compact")))
	case input == "/model":
		m.input.Reset()
		m.openModelPicker()
		return nil
	case strings.HasPrefix(input, "/model "):
		m.input.Reset()
		return m.runModelCommand(strings.TrimSpace(strings.TrimPrefix(input, "/model")))
	case input == "/login" || strings.HasPrefix(input, "/login "):
		m.input.Reset()
		return m.runLogin(providerArg(input, "/login"), "")
	case input == "/logout" || strings.HasPrefix(input, "/logout "):
		m.input.Reset()
		m.doLogout(providerArg(input, "/logout"))
		return nil
	}
	return m.submit(input)
}

// providerArg returns the provider named after a slash command, defaulting to
// the OpenAI provider.
func providerArg(input, cmd string) string {
	p := strings.TrimSpace(strings.TrimPrefix(input, cmd))
	if p == "" {
		return llm.OpenAIProviderID
	}
	return p
}

func (m *Model) insertImageMarker(count int) {
	marker := imageMarker(count) + " "
	if strings.TrimSpace(m.input.Value()) != "" {
		marker = " " + marker
	}
	m.input.InsertString(marker)
	if m.resizeInputHeight() && m.ready {
		m.layout()
	}
	m.syncCommandMenu()
	m.syncFileMenu()
}

func (m *Model) navHistory(delta int) {
	if len(m.history) == 0 {
		return
	}
	if m.histIdx == len(m.history) && delta < 0 {
		m.histDraft = m.input.Value()
	}
	idx := m.histIdx + delta
	if idx < 0 {
		idx = 0
	}
	if idx > len(m.history) {
		idx = len(m.history)
	}
	m.histIdx = idx
	if idx == len(m.history) {
		m.input.SetValue(m.histDraft)
	} else {
		m.input.SetValue(m.history[idx])
	}
	m.input.CursorEnd()
}

func (m *Model) submit(input string) tea.Cmd {
	images := append([]llm.Image(nil), m.pendingImages...)
	m.pendingImages = nil
	displayText := userTextWithImages(input, len(images))
	title := session.Title(input)
	if strings.TrimSpace(input) == "" {
		title = session.Title(imagesLabel(len(images)))
	}
	return m.startTurn(block{kind: bkUser, text: displayText}, title,
		func(ctx context.Context, ev agent.Events) (string, error) {
			return m.agent.RunWithImages(ctx, input, images, ev)
		})
}

// runSkill handles a "/skill:<name> [args]" command: it renders the skill and
// runs it as a turn, shown in history as a distinct skill line.
func (m *Model) runSkill(rest string) tea.Cmd {
	name, args, _ := strings.Cut(strings.TrimSpace(rest), " ")
	args = strings.TrimSpace(args)
	sk, ok := m.skills.Get(name)
	if !ok || !sk.UserInvocable {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "no such skill: " + name})
		m.refresh()
		return nil
	}
	display := strings.TrimSpace(name + " " + args)
	sid := m.sessionID
	return m.startTurn(block{kind: bkSkill, text: display}, session.Title("/skill:"+display),
		func(ctx context.Context, ev agent.Events) (string, error) {
			body, err := sk.Render(ctx, args, skill.RenderOpts{SessionID: sid})
			if err != nil {
				return "", err
			}
			return m.agent.Run(ctx, body, ev)
		})
}

// runMcpPrompt handles a "/mcp__<server>__<prompt> [args]" command: it fetches
// the prompt from the server and injects its messages as a turn.
func (m *Model) runMcpPrompt(rest string) tea.Cmd {
	name, args, _ := strings.Cut(strings.TrimSpace(rest), " ")
	args = strings.TrimSpace(args)
	display := strings.TrimSpace(name + " " + args)
	return m.startTurn(block{kind: bkMcpPrompt, text: display}, session.Title("/"+display),
		func(ctx context.Context, ev agent.Events) (string, error) {
			body, err := m.mcpMgr.RenderPrompt(ctx, name, args)
			if err != nil {
				return "", err
			}
			return m.agent.Run(ctx, body, ev)
		})
}

// runCompact handles "/compact [instructions]": it summarizes the conversation
// as a normal turn so the spinner and notices show, then the gauge re-renders.
func (m *Model) runCompact(instructions string) tea.Cmd {
	display := "/compact"
	if instructions != "" {
		display += " " + instructions
	}
	return m.startTurn(block{kind: bkNotice, text: display}, m.sessionTitle,
		func(ctx context.Context, ev agent.Events) (string, error) {
			// The status is surfaced via OnNotice during the run; returning it
			// again would duplicate the final notice as an assistant block.
			_, err := m.agent.Compact(ctx, instructions, ev)
			return "", err
		})
}

// startTurn appends the display block, marks the model busy, and runs `run` in a
// goroutine wired to the event bridge.
func (m *Model) startTurn(display block, title string,
	run func(context.Context, agent.Events) (string, error)) tea.Cmd {
	m.blocks = append(m.blocks, display)
	m.input.Reset()
	if m.resizeInputHeight() && m.ready {
		m.layout()
	}
	m.busy = true
	m.resetStream()
	if m.sessionID == "" {
		m.sessionStart = time.Now()
		m.sessionID = session.NewID(m.sessionStart)
		if m.sessionTitle == "" { // keep a SessionStart-hook title if one was set
			m.sessionTitle = title
		}
		if m.hooks != nil {
			m.hooks.SetSession(m.sessionID, "")
		}
		m.agent.SetSessionID(m.sessionID)
	}
	m.refresh()
	m.vp.GotoBottom() // a new turn re-anchors the view to the bottom

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	go func() {
		defer cancel()
		answer, err := run(ctx, m.agentEvents())
		m.events <- turnDoneMsg{answer: answer, err: err}
	}()
	return m.spin.Tick
}

// agentEvents bridges agent callbacks onto the TUI event channel.
func (m *Model) agentEvents() agent.Events {
	return agent.Events{
		OnContent:          func(d string) { m.events <- contentMsg(d) },
		OnAssistantMessage: func(c string) { m.events <- assistantStepMsg(c) },
		OnReasoning:        func(d string) { m.events <- reasoningMsg(d) },
		OnUsage:            func(t int) { m.events <- usageMsg(t) },
		OnTodoUpdate:       func(todos []agent.TodoItem) { m.events <- todoUpdateMsg(todos) },
		OnToolStart: func(name string, args json.RawMessage) {
			m.events <- toolStartMsg{name: name, args: string(args)}
		},
		OnToolEnd: func(name, result string, err error) {
			m.events <- toolEndMsg{name: name, result: result, err: err}
		},
		OnNotice: func(text string) { m.events <- noticeMsg(text) },
		OnAgentStart: func(id, ag, prompt string) {
			m.events <- agentStartMsg{id: id, agent: ag, prompt: prompt}
		},
		OnAgentEnd: func(id, result string, err error) {
			m.events <- agentEndMsg{id: id, result: result, err: err}
		},
		OnSubToolStart: func(id, ag, name string, args json.RawMessage) {
			m.events <- subToolStartMsg{id: id, agent: ag, name: name, args: string(args)}
		},
		OnSubToolEnd: func(id, ag, name, result string, err error) {
			m.events <- subToolEndMsg{id: id, agent: ag, name: name, result: result, err: err}
		},
		OnSubNotice: func(id, ag, text string) { m.events <- subNoticeMsg{id: id, agent: ag, text: text} },
	}
}

// hasRunningGroup reports whether any subagent run is still in progress; their
// header spinners already signal activity, so the bottom "Thinking" line is
// redundant while one is running.
func (m *Model) hasRunningGroup() bool {
	for _, g := range m.groups {
		if !g.done {
			return true
		}
	}
	return false
}

// appendToGroup adds a nested block to the named subagent run. If the run is
// unknown (e.g. its start event was lost), the block falls back to the main
// timeline so activity is never silently dropped.
func (m *Model) appendToGroup(id string, b block) {
	if g := m.groups[id]; g != nil {
		g.lines = append(g.lines, b)
		return
	}
	m.blocks = append(m.blocks, b)
}

// ---- project hook trust ----

// handleTrustKey resolves the startup prompt asking whether to run this
// project's local hooks. y trusts and persists; any other key leaves them off.
func (m *Model) handleTrustKey(msg tea.KeyMsg) tea.Cmd {
	m.trustAsk = false
	switch strings.ToLower(msg.String()) {
	case "y":
		if err := m.hooks.TrustProject(); err != nil {
			m.blocks = append(m.blocks, block{kind: bkError, text: "could not persist trust: " + err.Error()})
		} else {
			m.blocks = append(m.blocks, block{kind: bkNotice, text: "project hooks trusted and enabled"})
		}
	default:
		m.blocks = append(m.blocks, block{kind: bkNotice,
			text: "project hooks left disabled (restart and press y, or pass --trust-project-hooks)"})
	}
	m.layout()
	m.refresh()
	return nil
}

func (m Model) trustView() string {
	w := m.overlayInnerWidth()
	body := lipgloss.JoinVertical(lipgloss.Left,
		padLine(overlayTitleStyle.Render("Trust this project's hooks?"), w, cSurface0),
		padLine(overlayTextStyle.Render(truncate(m.hooks.ProjectDir(), w)), w, cSurface0),
		padLine(overlayTextStyle.Render("It defines hooks that run shell commands on agent events."), w, cSurface0),
		padLine(overlayHintStyle.Render("y trust (persisted) · any other key skip"), w, cSurface0),
	)
	return overlayBoxStyle.Render(body)
}

// ---- project skill trust ----

// handleSkillTrustKey resolves the startup prompt asking whether to load this
// project's local skills. y approves their current definitions and folds them
// into the running session; any other key leaves them out.
func (m *Model) handleSkillTrustKey(msg tea.KeyMsg) tea.Cmd {
	// Quitting is not a decision, so it takes the ordinary exit path rather than
	// falling through to the decline branch below.
	if msg.Type == tea.KeyCtrlC {
		m.skillAsk = nil
		cmd, _ := m.handleKey(msg)
		return cmd
	}
	m.skillAsk = nil
	switch strings.ToLower(msg.String()) {
	case "y":
		loaded, err := m.adoptProjectSkills()
		switch {
		case err != nil:
			m.blocks = append(m.blocks, block{kind: bkError, text: "could not trust project skills: " + err.Error()})
		case len(loaded) == 0:
			// Approval is recorded either way, so this is terminal, not retryable:
			// the definitions did not parse, or a same-named skill from a
			// higher-priority root already holds every one of their names.
			m.blocks = append(m.blocks, block{kind: bkNotice,
				text: "project skills approved, but none loaded - unreadable or shadowed by same-named skills"})
		default:
			m.blocks = append(m.blocks, block{kind: bkNotice,
				text: "project skills trusted and loaded: " + strings.Join(loaded, ", ")})
		}
	default:
		// Skipping persists nothing, so simply relaunching asks again; the flag is
		// only needed for the non-interactive front-ends that cannot ask.
		m.blocks = append(m.blocks, block{kind: bkNotice,
			text: "project skills not loaded; restart to be asked again"})
	}
	m.layout()
	m.refresh()
	return nil
}

// adoptProjectSkills approves the project's current skill definitions and makes
// the newly visible skills usable without a restart. The registry is swapped in
// place because the skill tool and the system-prompt builder hold that same
// pointer; everything else cached off the old set is rebuilt here.
// It returns the project-local skills that actually loaded, which is what the
// user should be told about rather than the names the prompt listed: approval
// re-fingerprints the files, so an edit in between can leave the set empty.
//
// Swapping the registry is only safe while no turn is running - the skill tool
// reads it from the agent goroutine and neither side is locked. The prompt owns
// the keyboard until it is dismissed, so nothing can have been submitted yet.
func (m *Model) adoptProjectSkills() ([]string, error) {
	cwd := m.skillAskCwd
	if cwd == "" {
		cwd = m.reg.Root()
	}
	if err := skill.ApproveProject(cwd); err != nil {
		return nil, err
	}
	found, errs := skill.Discover(cwd)
	for _, e := range errs {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "skipped skill: " + e.Error()})
	}
	if m.skills == nil {
		m.skills = found
	} else {
		m.skills.Replace(found)
	}
	if m.regSkillTool != nil {
		m.regSkillTool(m.skills)
	}
	m.agent.WatchSkills(m.skills.Conditional())
	m.commands = buildCommands(m.skills, m.mcpMgr)
	// The launch-time system prompt listed only the trusted skills, so without
	// this the model still cannot see the ones just approved.
	if m.rebuildSystem != nil {
		m.agent.SetSystem(m.rebuildSystem())
	}
	var loaded []string
	for _, s := range m.skills.List() {
		if s.ProjectLocal {
			loaded = append(loaded, skill.DisplaySafe(s.Name))
		}
	}
	return loaded, nil
}

// skillTrustNameLines lays the skill names out over at most maxLines lines and
// says how many did not fit. The names are the substance of the decision, so
// nothing may be dropped without being counted: it packs names itself rather
// than re-parsing wrapped text (a name may contain the separator, and would
// otherwise be miscounted or read as several entries), and measures terminal
// cells rather than runes, since a repo can pick names whose runes are double
// width or a single name longer than the whole box.
func skillTrustNameLines(names []string, w, maxLines int) []string {
	if maxLines < 2 {
		maxLines = 2
	}
	if len(names) == 0 {
		return []string{"(no readable skill definitions)"}
	}
	// pack fills at most limit lines and reports how many names they hold.
	pack := func(limit int) ([]string, int) {
		var lines []string
		cur, held := "", 0
		for _, name := range names {
			n := ansi.Truncate(name, w, "…")
			cand := n
			if cur != "" {
				cand = cur + ", " + n
			}
			if ansi.StringWidth(cand) > w {
				lines = append(lines, cur)
				if len(lines) == limit {
					return lines, held
				}
				cand = n
			}
			cur, held = cand, held+1
		}
		if cur != "" {
			lines = append(lines, cur)
		}
		return lines, held
	}
	if lines, held := pack(maxLines); held == len(names) {
		return lines
	}
	lines, held := pack(maxLines - 1)
	return append(lines, fmt.Sprintf("... and %d more", len(names)-held))
}

func (m Model) skillTrustView() string {
	w := m.overlayInnerWidth()
	title := "Load this project's skills?"
	if m.skillAsk.Invalidated {
		title = "Reload this project's changed skills?"
	}
	// The count is the one number a crafted name cannot forge, so it leads.
	rows := []string{
		padLine(overlayTitleStyle.Render(title), w, cSurface0),
		padLine(overlayTextStyle.Render(ansi.Truncate(m.skillAsk.Dir, w, "…")), w, cSurface0),
		padLine(overlayTextStyle.Render(fmt.Sprintf("%d skill(s):", len(m.skillAsk.Names))), w, cSurface0),
	}
	for _, l := range skillTrustNameLines(m.skillAsk.Names, w, 4) {
		rows = append(rows, padLine(overlayTextStyle.Render(l), w, cSurface0))
	}
	// Naming allowed-tools matters: it is the one power that bypasses the
	// confirmation box the user would otherwise get for each call.
	detail := "They run shell commands and hooks, and can pre-approve tools for a turn."
	if m.skillAsk.Invalidated {
		detail = "Their definitions changed since you approved them. " + detail
	}
	for _, l := range wrapText(detail, w) {
		rows = append(rows, padLine(overlayTextStyle.Render(l), w, cSurface0))
	}
	hint := "y trust (persisted) · ctrl+c quit · any other key skip"
	rows = append(rows, padLine(overlayHintStyle.Render(hint), w, cSurface0))
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// ---- confirmation ----

func (m *Model) handleConfirmReq(msg confirmReqMsg) {
	// A path request is never settled by a session tool policy or by auto mode:
	// those govern which tools may run, and leaving the working directory is a
	// separate question that only the user answers.
	if msg.pathResp == nil {
		switch m.toolPolicy[msg.name] {
		case "allow":
			msg.resp <- true
			return
		case "deny":
			msg.resp <- false
			return
		}
		// Auto mode approves anything that is reversible from the code (edits, safe
		// commands) without prompting; an irreversible destructive op still asks.
		if m.autoMode && !tools.IsDestructive(msg.name, json.RawMessage(msg.args)) {
			msg.resp <- true
			return
		}
	}
	// Concurrent subagents can request confirmation at once; show one box and
	// queue the rest.
	if m.pending != nil {
		m.pendingQueue = append(m.pendingQueue, &msg)
		return
	}
	m.pending = &msg
	m.confirmIdx = choiceOnce
	if m.hooks != nil {
		go m.hooks.RunBounded(hooks.EventNotification,
			hooks.Input{Message: "awaiting confirmation for tool: " + msg.name}, 30*time.Second)
	}
	m.layout()
}

// confirmOptions returns the labels of the pending dialog, in selection order.
// The last one is always the refusal, so Esc and the deny key work without
// knowing which dialog is showing.
func (m Model) confirmOptions() []string {
	if m.pending == nil {
		return nil
	}
	switch {
	case m.pending.pathResp == nil:
		return []string{"Once", "Always", "Forbid"}
	case m.pending.write:
		// A write outside the working directory is never remembered, so an
		// "Always" here would be a button that does not do what it says.
		return []string{"Once", "Deny"}
	default:
		return []string{"Once", "Always (this folder)", "Deny"}
	}
}

func (m *Model) handleConfirmKey(msg tea.KeyMsg) tea.Cmd {
	last := confirmChoice(len(m.confirmOptions()) - 1)
	switch msg.Type {
	case tea.KeyLeft:
		if m.confirmIdx > choiceOnce {
			m.confirmIdx--
		}
		m.refresh()
	case tea.KeyRight, tea.KeyTab:
		if m.confirmIdx < last {
			m.confirmIdx++
		}
		m.refresh()
	case tea.KeyEnter:
		m.answerConfirm(m.confirmIdx)
	case tea.KeyEsc:
		m.answerConfirm(last)
	case tea.KeyRunes:
		switch strings.ToLower(string(msg.Runes)) {
		case "y", "o", "1":
			m.answerConfirm(choiceOnce)
		case "a", "2":
			// "Always" is the middle option only when there is one; on a write
			// request the same key must not fall through to a refusal.
			if last > choiceAlways {
				m.answerConfirm(choiceAlways)
			}
		case "n", "f", "3":
			m.answerConfirm(last)
		}
	}
	return nil
}

func (m *Model) answerConfirm(choice confirmChoice) {
	pending := m.pending
	if pending.pathResp != nil {
		m.answerPathReq(pending, choice)
	} else {
		name := pending.name
		ok := choice != choiceForbid
		switch choice {
		case choiceAlways:
			m.toolPolicy[name] = "allow"
		case choiceForbid:
			m.toolPolicy[name] = "deny"
		}
		pending.resp <- ok
	}
	m.pending = nil
	m.promoteNextConfirm()
	m.layout()
}

// answerPathReq resolves an out-of-root path request. "Always" records the
// path's directory for this project, so the rest of that tree is read without
// asking again - here and in later sessions.
func (m *Model) answerPathReq(req *confirmReqMsg, choice confirmChoice) {
	last := confirmChoice(len(m.confirmOptions()) - 1)
	switch {
	case choice >= last:
		req.pathResp <- tools.PathDeny
	case choice == choiceOnce:
		req.pathResp <- tools.PathAllowOnce
	default:
		req.pathResp <- tools.PathAllowDir
		m.blocks = append(m.blocks, block{kind: bkNotice,
			text: "allowed " + filepath.Dir(req.path) + " for this project"})
	}
}

// pathGranted reports whether a queued path request is already covered - the
// user may have just approved a directory above it, and asking twice for the
// same tree is exactly what "Always" was meant to prevent.
func (m *Model) pathGranted(req *confirmReqMsg) bool {
	if req.write || m.reg == nil {
		return false // a write is never covered by a grant
	}
	ok, err := pathgrant.Allowed(m.reg.Root(), req.path)
	return err == nil && ok
}

// promoteNextConfirm shows the next queued confirmation, auto-resolving any that
// a session policy now covers (e.g. set by an "Always"/"Forbid" just chosen).
func (m *Model) promoteNextConfirm() {
	for len(m.pendingQueue) > 0 {
		next := m.pendingQueue[0]
		m.pendingQueue = m.pendingQueue[1:]
		if next.pathResp != nil {
			if m.pathGranted(next) {
				next.pathResp <- tools.PathAllowOnce
				continue
			}
			m.pending = next
			m.confirmIdx = choiceOnce
			return
		}
		switch m.toolPolicy[next.name] {
		case "allow":
			next.resp <- true
		case "deny":
			next.resp <- false
		default:
			if m.autoMode && !tools.IsDestructive(next.name, json.RawMessage(next.args)) {
				next.resp <- true
				continue
			}
			m.pending = next
			m.confirmIdx = choiceOnce
			return
		}
	}
}

// ---- resume picker ----

func (m *Model) openPicker() {
	metas, err := session.List()
	if err != nil {
		m.blocks = append(m.blocks, block{kind: bkError, text: "list sessions: " + err.Error()})
		m.refresh()
		return
	}
	if len(metas) == 0 {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "no saved sessions yet"})
		m.refresh()
		return
	}
	m.picker = &resumePicker{items: metas}
	m.layout()
}

func (m *Model) handlePickerKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.Type {
	case tea.KeyUp:
		if m.picker.cursor > 0 {
			m.picker.cursor--
		}
		m.refresh()
	case tea.KeyDown:
		if m.picker.cursor < len(m.picker.items)-1 {
			m.picker.cursor++
		}
		m.refresh()
	case tea.KeyEnter:
		m.loadSession(m.picker.items[m.picker.cursor].ID)
		m.picker = nil
		m.layout()
	case tea.KeyEsc:
		m.picker = nil
		m.layout()
	}
	return nil
}

// ---- skills browser ----

func (m *Model) openSkills() {
	if m.skills == nil || m.skills.Len() == 0 {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "no skills found"})
		m.refresh()
		return
	}
	m.browser = &skillBrowser{items: m.skills.List()}
	m.layout()
}

func (m *Model) handleSkillKey(msg tea.KeyMsg) tea.Cmd {
	b := m.browser
	switch msg.Type {
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
		sk := b.items[b.cursor]
		if !b.detail {
			b.detail = true // first Enter opens detail
			m.refresh()
			return nil
		}
		// Enter in detail runs the skill (if user-invocable).
		m.browser = nil
		m.layout()
		if sk.UserInvocable {
			return m.runSkill(sk.Name)
		}
		m.blocks = append(m.blocks, block{kind: bkNotice, text: sk.Name + " is not user-invocable"})
		m.refresh()
	case tea.KeyEsc:
		if b.detail {
			b.detail = false // back to the list
			m.refresh()
			return nil
		}
		m.browser = nil
		m.layout()
	}
	return nil
}

// ---- mcp browser ----

func (m *Model) openMcp() {
	if m.mcpMgr.Empty() {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "no MCP servers configured"})
		m.refresh()
		return
	}
	b := &mcpBrowser{servers: m.mcpMgr.Servers()}
	for _, sv := range b.servers {
		for _, r := range sv.Resources {
			if r.Template {
				continue // templates need a filled URI; not previewable as-is
			}
			b.items = append(b.items, mcpResItem{server: sv.Name, uri: r.URI, name: r.Name})
		}
	}
	m.mcp = b
	m.layout()
}

func (m *Model) handleMcpKey(msg tea.KeyMsg) tea.Cmd {
	b := m.mcp
	if b.preview != "" || b.loading {
		if msg.Type == tea.KeyEsc {
			b.preview, b.loading = "", false
			m.layout()
		}
		return nil
	}
	switch msg.Type {
	case tea.KeyUp:
		if b.cursor > 0 {
			b.cursor--
		}
		m.refresh()
	case tea.KeyDown:
		if b.cursor < len(b.items)-1 {
			b.cursor++
		}
		m.refresh()
	case tea.KeyEnter:
		if b.cursor < len(b.items) {
			b.loading = true
			m.layout()
			return m.loadMcpPreview(b.items[b.cursor])
		}
	case tea.KeyEsc:
		m.mcp = nil
		m.layout()
	}
	return nil
}

// loadMcpPreview reads a resource off the UI goroutine and reports it back.
func (m *Model) loadMcpPreview(it mcpResItem) tea.Cmd {
	mgr := m.mcpMgr
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		text, err := mgr.ReadResource(ctx, it.server, it.uri)
		return mcpPreviewMsg{text: text, err: err}
	}
}

// ---- command menu ----

// buildCommands lists the slash commands offered by the autocomplete menu: the
// built-in commands, one entry per user-invocable skill, and (when MCP servers
// are connected) the /mcp browser plus one entry per MCP prompt.
func buildCommands(skills *skill.Registry, mcpMgr *mcp.Manager) []commandItem {
	cmds := []commandItem{
		{name: "/new", desc: "Start a fresh session (clears the conversation)"},
		{name: "/model", desc: "Switch the active model"},
		{name: "/login", desc: "Authenticate a provider (e.g. /login openai)"},
		{name: "/logout", desc: "Clear a provider's stored credential"},
		{name: "/resume", desc: "Resume a saved session"},
		{name: "/skills", desc: "Browse available skills"},
		{name: "/agents", desc: "Browse and configure agents"},
		{name: "/artifacts", desc: "Review files changed this session"},
		{name: "/compact", desc: "Summarize the conversation to free context"},
	}
	if skills != nil {
		for _, s := range skills.List() {
			if s.UserInvocable {
				cmds = append(cmds, commandItem{name: "/skill:" + s.Name, desc: oneLine(s.Description)})
			}
		}
	}
	if !mcpMgr.Empty() {
		cmds = append(cmds, commandItem{name: "/mcp", desc: "Browse MCP servers and resources"})
		for _, p := range mcpMgr.PromptCommands() {
			desc := oneLine(p.Desc)
			if len(p.Args) > 0 {
				desc = "args: " + strings.Join(p.Args, " ") + "  " + desc
			}
			cmds = append(cmds, commandItem{name: "/" + p.Name, desc: desc})
		}
	}
	return cmds
}

// filterCommands ranks commands against the typed query with a fuzzy match.
// A bare "/" (or empty) shows every command in definition order.
func filterCommands(cmds []commandItem, query string) []commandItem {
	if query == "" || query == "/" {
		return cmds
	}
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.name
	}
	out := make([]commandItem, 0, len(cmds))
	for _, mt := range fuzzy.Find(query, names) {
		out = append(out, cmds[mt.Index])
	}
	return out
}

// syncCommandMenu opens, updates, or closes the autocomplete menu based on the
// current input: it shows while the line is a slash command still being typed
// (starts with '/', single token, no space) and there is at least one match.
func (m *Model) syncCommandMenu() {
	val := m.input.Value()
	if !strings.HasPrefix(val, "/") || strings.ContainsAny(val, " \n") {
		m.closeCommandMenu()
		return
	}
	matches := filterCommands(m.commands, val)
	if len(matches) == 0 {
		m.closeCommandMenu()
		return
	}
	if m.cmdMenu == nil {
		m.cmdMenu = &commandMenu{}
	}
	m.cmdMenu.items = matches
	if m.cmdMenu.cursor >= len(matches) {
		m.cmdMenu.cursor = len(matches) - 1
	}
	if m.cmdMenu.cursor < 0 {
		m.cmdMenu.cursor = 0
	}
	m.layout()
}

func (m *Model) closeCommandMenu() {
	if m.cmdMenu != nil {
		m.cmdMenu = nil
		m.layout()
	}
}

func (m *Model) handleCmdMenuKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	menu := m.cmdMenu
	switch msg.Type {
	case tea.KeyUp:
		if menu.cursor > 0 {
			menu.cursor--
		}
		m.refresh()
		return nil, true
	case tea.KeyDown:
		if menu.cursor < len(menu.items)-1 {
			menu.cursor++
		}
		m.refresh()
		return nil, true
	case tea.KeyTab:
		m.input.SetValue(menu.items[menu.cursor].name)
		m.input.CursorEnd()
		m.syncCommandMenu()
		return nil, true
	case tea.KeyEnter:
		if m.busy {
			return nil, true
		}
		name := menu.items[menu.cursor].name
		m.input.SetValue(name)
		m.closeCommandMenu()
		return m.dispatch(name), true
	case tea.KeyEsc:
		m.closeCommandMenu()
		return nil, true
	}
	return nil, false
}

func (m Model) cmdMenuView() string {
	w := m.overlayInnerWidth()
	const maxRows = 8
	menu := m.cmdMenu
	start := 0
	if menu.cursor >= maxRows {
		start = menu.cursor - maxRows + 1
	}
	nameW := max(8, w/3)
	title := overlayTitleStyle.Render("Commands  ") +
		overlayHintStyle.Render("(↑/↓ · Tab complete · Enter run · Esc close)")
	rows := []string{padLine(title, w, cSurface0)}
	for i := start; i < len(menu.items) && i < start+maxRows; i++ {
		c := menu.items[i]
		line := fmt.Sprintf(" %-*s  %s", nameW, truncate(c.name, nameW),
			truncate(c.desc, max(8, w-nameW-4)))
		if i == menu.cursor {
			rows = append(rows, pickSelStyle.Width(w).MaxWidth(w).Render(line))
		} else {
			rows = append(rows, pickRowStyle.Width(w).MaxWidth(w).Render(line))
		}
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// ---- model picker / login / logout ----

// openModelPicker builds the /model list: every registry model grouped by
// provider, with a lock on models whose provider is not authenticated.
func (m *Model) openModelPicker() {
	if m.modelReg == nil {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "no model registry available"})
		m.refresh()
		return
	}
	models := m.modelReg.Models()
	llm.SortModelsByRef(models)
	cur := m.backend.Model().Ref()
	mp := &modelPicker{}
	for _, mi := range models {
		p, _, err := m.modelReg.Resolve(mi.Ref())
		locked := err == nil && p.NeedsAuth() && !auth.IsAuthenticated(p.ID)
		mp.all = append(mp.all, modelItem{
			ref: mi.Ref(), name: mi.Name, provider: mi.Provider,
			locked: locked, current: mi.Ref() == cur,
		})
	}
	mp.items = mp.all
	for i, it := range mp.all {
		if it.current {
			mp.cursor = i
		}
	}
	m.models = mp
	m.layout()
}

// filter narrows the model list against the typed query.
func (mp *modelPicker) filter() {
	if mp.query == "" {
		mp.items = mp.all
	} else {
		names := make([]string, len(mp.all))
		for i, it := range mp.all {
			names[i] = it.ref + " " + it.name
		}
		out := make([]modelItem, 0, len(mp.all))
		for _, mt := range fuzzy.Find(mp.query, names) {
			out = append(out, mp.all[mt.Index])
		}
		mp.items = out
	}
	if mp.cursor >= len(mp.items) {
		mp.cursor = max(0, len(mp.items)-1)
	}
}

func (m *Model) handleModelKey(msg tea.KeyMsg) tea.Cmd {
	mp := m.models
	switch msg.Type {
	case tea.KeyUp:
		if mp.cursor > 0 {
			mp.cursor--
		}
		m.refresh()
	case tea.KeyDown:
		if mp.cursor < len(mp.items)-1 {
			mp.cursor++
		}
		m.refresh()
	case tea.KeyEnter:
		if len(mp.items) == 0 {
			return nil
		}
		it := mp.items[mp.cursor]
		m.models = nil
		m.layout()
		if it.locked {
			m.blocks = append(m.blocks, block{kind: bkNotice, text: it.provider + " needs login"})
			m.refresh()
			return m.runLogin(it.provider, it.ref)
		}
		if it.provider == llm.LocalProviderID {
			m.localChoice = &localModelChoice{ref: it.ref}
			m.layout()
			return nil
		}
		m.switchModel(it.ref, true)
	case tea.KeyEsc:
		m.models = nil
		m.layout()
	case tea.KeyBackspace:
		if mp.query != "" {
			mp.query = mp.query[:len(mp.query)-1]
			mp.filter()
			m.refresh()
		}
	case tea.KeyRunes:
		mp.query += string(msg.Runes)
		mp.filter()
		m.refresh()
	}
	return nil
}

// switchModel rebuilds the backend for ref and redirects the live session (and
// its tools) to it via the shared Ref, updating the gauge and compaction window.
// History is provider-neutral, so the conversation continues uninterrupted.
// switchModel swaps the live backend to ref. persist records it as the user's
// preferred model for next startup - true when the user explicitly picks a model,
// false when restoring a resumed session's model (which must not redefine the
// global default).
func (m *Model) switchModel(ref string, persist bool) {
	b, _, info, err := auth.OpenModel(m.modelReg, ref, m.maxTokens)
	if err != nil {
		m.blocks = append(m.blocks, block{kind: bkError, text: "switch model: " + err.Error()})
		m.refresh()
		return
	}
	m.backend.Set(b)
	m.model = info.Ref()
	m.url = b.Endpoint()
	if persist {
		_ = config.SaveModelPref(info.Ref()) // best-effort; a failed write must not interrupt
	}

	// A model without its own context window falls back to the startup default,
	// never silently keeps the previous model's window.
	cw := info.ContextWindow
	if cw <= 0 {
		cw = m.defaultCtxSize
	}
	m.ctxSize = cw
	m.compactCfg.CtxSize = cw
	m.agent.SetCompaction(m.compactCfg)
	m.ctxTokens = m.agent.ContextTokens()
	m.blocks = append(m.blocks, block{kind: bkNotice, text: "switched to " + info.Ref()})
	m.refresh()
}

// localWizard steps the user through local-model setup inside the TUI.
type localWizard struct {
	step    int
	cfg     local.Config // defaults; mutated as the user answers
	srcCur  int          // step 0 cursor: 0 = HF, 1 = path
	value   string       // step 1 text: repo (HF) or path
	binary  string       // step 2 text
	errText string       // last validation error, shown on confirm
}

const (
	wizStepSource  = 0
	wizStepValue   = 1
	wizStepBinary  = 2
	wizStepConfirm = 3
)

func (w *localWizard) init() {
	w.cfg = local.Defaults()
	w.step = wizStepSource
	w.srcCur = 0
	w.value = w.cfg.HFRepo // prefill with default repo
	w.binary = w.cfg.BinaryPath
}

// config materializes the wizard answers into a local.Config (no I/O).
func (w *localWizard) config() local.Config {
	c := w.cfg
	if w.srcCur == 1 {
		c.SourceKind = local.SourcePath
		c.ModelPath = strings.TrimSpace(w.value)
		if c.ModelPath != "" {
			c.ModelName = filepath.Base(c.ModelPath)
		}
	} else {
		c.SourceKind = local.SourceHF
		if r := strings.TrimSpace(w.value); r != "" {
			c.HFRepo = r
		}
	}
	if b := strings.TrimSpace(w.binary); b != "" {
		c.BinaryPath = b
	}
	return c
}

// handleKey advances the wizard through the text-entry steps. The confirm step
// is handled by the caller (handleLocalWizardKey), which saves and starts.
func (w *localWizard) handleKey(msg tea.KeyMsg) {
	switch w.step {
	case wizStepSource:
		switch msg.Type {
		case tea.KeyUp:
			if w.srcCur > 0 {
				w.srcCur--
			}
		case tea.KeyDown:
			if w.srcCur < 1 {
				w.srcCur++
			}
		case tea.KeyEnter:
			// Prefill the value field based on the chosen source.
			if w.srcCur == 1 {
				w.value = ""
			} else {
				w.value = w.cfg.HFRepo
			}
			w.step = wizStepValue
		}
	case wizStepValue:
		switch msg.Type {
		case tea.KeyEnter:
			w.step = wizStepBinary
		case tea.KeyBackspace:
			if w.value != "" {
				w.value = w.value[:len(w.value)-1]
			}
		case tea.KeyRunes:
			w.value += string(msg.Runes)
		}
	case wizStepBinary:
		switch msg.Type {
		case tea.KeyEnter:
			w.step = wizStepConfirm
		case tea.KeyBackspace:
			if w.binary != "" {
				w.binary = w.binary[:len(w.binary)-1]
			}
		case tea.KeyRunes:
			w.binary += string(msg.Runes)
		}
	}
}

// openLocalWizard opens the local-model setup overlay seeded with defaults.
func (m *Model) openLocalWizard() {
	w := &localWizard{}
	w.init()
	m.localWiz = w
	m.layout()
}

// handleLocalChoiceKey drives the local-model action widget: ←/→ or Tab move
// focus, Enter (or the u/c/d shortcuts) confirm the focused action.
func (m *Model) handleLocalChoiceKey(msg tea.KeyMsg) tea.Cmd {
	lc := m.localChoice
	switch msg.Type {
	case tea.KeyLeft:
		if lc.idx > 0 {
			lc.idx--
		}
		m.refresh()
	case tea.KeyRight, tea.KeyTab:
		if lc.idx < 2 {
			lc.idx++
		}
		m.refresh()
	case tea.KeyEnter:
		return m.applyLocalChoice(lc.ref, lc.idx)
	case tea.KeyEsc:
		m.closeLocalChoice()
	case tea.KeyRunes:
		switch strings.ToLower(string(msg.Runes)) {
		case "u":
			return m.applyLocalChoice(lc.ref, 0)
		case "c":
			return m.applyLocalChoice(lc.ref, 1)
		case "d":
			return m.applyLocalChoice(lc.ref, 2)
		}
	}
	return nil
}

// closeLocalChoice dismisses the widget and re-lays out so the chat reclaims the
// rows the overlay occupied.
func (m *Model) closeLocalChoice() {
	m.localChoice = nil
	m.layout()
}

// applyLocalChoice runs the focused action: 0 use, 1 configure, 2 drop. Drop is
// irreversible (it deletes downloaded files), so it asks for confirmation first.
func (m *Model) applyLocalChoice(ref string, idx int) tea.Cmd {
	m.closeLocalChoice()
	switch idx {
	case 0:
		return m.selectLocal(ref)
	case 1:
		m.openLocalWizard()
	case 2:
		m.alert = &alertBox{
			title:        "Drop local model?",
			body:         "Stops llama.cpp and permanently deletes the downloaded model files.",
			confirmLabel: "Delete",
			action:       alertLocalDrop,
		}
		m.layout()
	}
	return nil
}

// dropLocal deletes the local model setup and files. If the local model is the
// live backend, it switches to another available model so the next turn does not
// hit the now-stopped llama.cpp server.
func (m *Model) dropLocal() tea.Cmd {
	wasActive := m.activeIsLocal()
	if err := local.Drop(); err != nil {
		m.blocks = append(m.blocks, block{kind: bkError, text: "drop local model: " + err.Error()})
		m.refresh()
		return nil
	}
	m.blocks = append(m.blocks, block{kind: bkNotice, text: "dropped local model setup and downloaded files"})
	if wasActive {
		if def, ok := m.fallbackModel(); ok {
			m.switchModel(def.Ref(), true)
		} else {
			m.blocks = append(m.blocks, block{kind: bkNotice,
				text: "no other model is available - configure one with /model"})
		}
	}
	m.refresh()
	return nil
}

// activeIsLocal reports whether the live backend is the local provider.
func (m *Model) activeIsLocal() bool {
	prov, _, _ := strings.Cut(m.backend.Model().Ref(), "/")
	return prov == llm.LocalProviderID
}

// fallbackModel returns a non-local model to switch to after dropping local,
// preferring an authenticated provider.
func (m *Model) fallbackModel() (llm.ModelInfo, bool) {
	if m.modelReg == nil {
		return llm.ModelInfo{}, false
	}
	if def, ok := m.modelReg.DefaultPreferring(auth.IsAuthenticated); ok &&
		def.Provider != llm.LocalProviderID {
		return def, true
	}
	return llm.ModelInfo{}, false
}

// selectLocal switches to the local model when it is available, otherwise shows
// the warning box with a download/setup confirm button.
func (m *Model) selectLocal(ref string) tea.Cmd {
	if a := assessActiveModel(ref); a != nil {
		m.alert = a
		m.layout()
		return nil
	}
	m.switchModel(ref, true)
	return nil
}

// handleLocalWizardKey routes overlay keys: Esc cancels, Enter on the confirm
// step saves and starts, all other keys advance the wizard.
func (m *Model) handleLocalWizardKey(msg tea.KeyMsg) tea.Cmd {
	w := m.localWiz
	if msg.Type == tea.KeyEsc {
		m.localWiz = nil
		m.layout()
		return nil
	}
	if w.step == wizStepConfirm && msg.Type == tea.KeyEnter {
		cfg := w.config()
		if err := cfg.Validate(); err != nil {
			w.errText = err.Error()
			m.refresh()
			return nil
		}
		if err := local.Save(cfg); err != nil {
			w.errText = err.Error()
			m.refresh()
			return nil
		}
		m.localWiz = nil
		m.layout()
		return m.startLocal(cfg)
	}
	w.handleKey(msg)
	m.refresh()
	return nil
}

// runModelCommand handles "/model <sub>"; an unknown subcommand opens the picker.
func (m *Model) runModelCommand(arg string) tea.Cmd {
	switch arg {
	case "init":
		m.openLocalWizard()
		return nil
	case "status":
		m.showLocalStatus()
		return nil
	case "start":
		cfg, exists, _ := local.Load()
		if !exists {
			m.blocks = append(m.blocks, block{kind: bkNotice, text: "local model not initialized - /model init"})
			m.refresh()
			return nil
		}
		return m.startLocal(cfg)
	case "stop":
		if err := local.Stop(); err != nil {
			m.blocks = append(m.blocks, block{kind: bkError, text: "stop: " + err.Error()})
		} else {
			m.blocks = append(m.blocks, block{kind: bkNotice, text: "stopped llama-server"})
		}
		m.refresh()
		return nil
	case "reset":
		if err := local.Reset(); err != nil {
			m.blocks = append(m.blocks, block{kind: bkError, text: "reset: " + err.Error()})
		} else {
			m.blocks = append(m.blocks, block{kind: bkNotice, text: "cleared local setup - /model init to set up again"})
		}
		m.refresh()
		return nil
	default:
		m.openModelPicker()
		return nil
	}
}

// startLocal launches the local daemon in the background, streaming download/
// load progress into one in-place block (model name + progress bar), then
// switches to it.
func (m *Model) startLocal(cfg local.Config) tea.Cmd {
	m.busy = true
	m.dlName = cfg.ModelName
	m.blocks = append(m.blocks, block{kind: bkDownload, text: m.downloadBlockText(local.Progress{Phase: local.PhaseLoading})})
	m.localProgIdx = len(m.blocks) - 1
	m.refresh()
	events, done := m.events, m.done
	return func() tea.Msg {
		err := local.Start(context.Background(), cfg, func(p local.Progress) {
			select {
			case events <- localProgressMsg(p):
			case <-done:
			}
		})
		return localStartedMsg{cfg: cfg, err: err}
	}
}

// downloadBlockText renders the live download block: the model name, a progress
// bar with percentage when the size is known, and the byte/speed detail. The
// caller sets m.prog.Width beforehand.
func (m *Model) downloadBlockText(p local.Progress) string {
	head := dlHeadStyle.Render("⬇ downloading " + m.dlName)
	frac := p.Fraction()
	switch {
	case p.Phase == local.PhaseLoading && frac < 0:
		return head + "\n" + overlayHintStyle.Render("  loading into memory...")
	case frac < 0:
		return head + "\n" + overlayHintStyle.Render("  "+p.Detail())
	case p.Phase == local.PhaseLoading:
		return head + "\n  " + m.prog.ViewAs(frac) + "  " + overlayHintStyle.Render(p.Detail()+" · loading into memory...")
	default:
		return head + "\n  " + m.prog.ViewAs(frac) + "  " + overlayHintStyle.Render(p.Detail())
	}
}

// downloadDoneText renders the finished download block: a full 100% bar, shown
// once the server is confirmed up. It works even when the size was unknown or
// the model came from cache, where no live frame ever reaches a full bar.
func (m *Model) downloadDoneText() string {
	m.prog.Width = max(20, min(48, m.width-30))
	return dlHeadStyle.Render("✓ "+m.dlName+" ready") + "\n  " + m.prog.ViewAs(1)
}

// handleAlertKey runs the warning box's confirm action on Enter, or dismisses it.
func (m *Model) handleAlertKey(msg tea.KeyMsg) tea.Cmd {
	a := m.alert
	switch msg.Type {
	case tea.KeyEnter:
		m.alert = nil
		m.layout()
		switch a.action {
		case alertLocalSetup:
			m.openLocalWizard()
		case alertLocalStart:
			return m.startLocal(a.cfg)
		case alertLocalDrop:
			return m.dropLocal()
		case alertLogin:
			return m.runLogin(a.provider, m.model)
		}
		return nil
	case tea.KeyEsc:
		m.alert = nil
		m.layout()
	}
	return nil
}

// localChoiceView renders the action widget shown after picking a local model.
// Each action carries a one-line explanation under the buttons; the height is
// constant across focus changes so the layout never shifts as Tab moves.
func (m Model) localChoiceView() string {
	w := m.overlayInnerWidth()
	labels := []string{"Use", "Configure", "Drop"}
	hints := []string{
		"Switch to this local model, starting llama.cpp if it is not running",
		"Re-run setup: model source, repo or path, and llama-server binary",
		"Stop llama.cpp and delete the downloaded model files (HF cache)",
	}
	opts := make([]string, len(labels))
	for i, label := range labels {
		if i == m.localChoice.idx {
			opts[i] = optSelStyle.Render(label)
		} else {
			opts[i] = optStyle.Render(label)
		}
	}
	hintStyle := overlayTextStyle
	if m.localChoice.idx == 2 {
		hintStyle = overlayWarnStyle
	}
	rows := []string{
		padLine(overlayTitleStyle.Render("Local model"), w, cSurface0),
		padLine(overlayTextStyle.Render(truncate(m.localChoice.ref, max(8, w-2))), w, cSurface0),
		padLine(strings.Join(opts, overlayTextStyle.Render(" ")), w, cSurface0),
		padLine(hintStyle.Render(truncate(hints[m.localChoice.idx], max(8, w-2))), w, cSurface0),
		padLine(overlayHintStyle.Render("←/→ or Tab select · Enter confirm · Esc cancel"), w, cSurface0),
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// alertView renders the active-model-unavailable warning box.
func (m Model) alertView() string {
	w := m.overlayInnerWidth()
	a := m.alert
	hint := "Esc dismiss"
	if a.confirmLabel != "" {
		hint = "Enter " + a.confirmLabel + " · Esc dismiss"
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		padLine(overlayTitleStyle.Render("⚠ "+a.title), w, cSurface0),
		padLine(overlayTextStyle.Render(truncate(a.body, max(8, w-2))), w, cSurface0),
		padLine(overlayHintStyle.Render(hint), w, cSurface0),
	)
	return overlayBoxStyle.Render(body)
}

// anyOverlayOpen reports whether any overlay is currently showing (the alert
// itself excluded - callers check that separately).
func (m *Model) anyOverlayOpen() bool {
	return m.trustAsk || m.skillAsk != nil || m.pending != nil || m.picker != nil || m.browser != nil ||
		m.agentBr != nil || m.mcp != nil || m.localWiz != nil || m.localChoice != nil || m.models != nil ||
		m.artBr != nil || m.cmdMenu != nil || m.fileMenu != nil
}

// reconcileFocus keeps the chat input focused only while no modal dialog is
// showing. A modal owns the keyboard, so the blinking input cursor is misleading
// there - blurring drops it. The command and file menus are excluded: they are
// autocomplete popups driven by the live input text, so the input must stay
// focused and editable while they show. Returns the blink command to re-issue
// when focus is restored (Focus must be re-armed for the cursor to blink again).
func (m *Model) reconcileFocus() tea.Cmd {
	modal := m.trustAsk || m.skillAsk != nil || m.pending != nil || m.alert != nil || m.picker != nil ||
		m.browser != nil || m.agentBr != nil || m.mcp != nil || m.localWiz != nil ||
		m.localChoice != nil || m.models != nil || m.artBr != nil
	if modal {
		if m.input.Focused() {
			m.input.Blur()
		}
		return nil
	}
	if !m.input.Focused() {
		return m.input.Focus()
	}
	return nil
}

// showLocalStatus prints a one-line local server status notice.
func (m *Model) showLocalStatus() {
	cfg, _, _ := local.Load()
	r := local.Status(cfg)
	m.blocks = append(m.blocks, block{kind: bkNotice, text: fmt.Sprintf(
		"local: initialized=%t running=%t reachable=%t url=%s", r.Initialized, r.Running, r.Reachable, cfg.BaseURL())})
	m.refresh()
}

// runLogin starts an interactive login for provider as a background command,
// surfacing a loginDoneMsg when it finishes. Only the OpenAI ChatGPT OAuth flow
// is interactive; API keys are configured via the CLI / env.
func (m *Model) runLogin(provider, thenSwitch string) tea.Cmd {
	if auth.IsAuthenticated(provider) {
		m.blocks = append(m.blocks, block{kind: bkNotice,
			text: provider + " is already authenticated (/logout " + provider + " to change)"})
		m.refresh()
		return nil
	}
	if provider != llm.OpenAIProviderID {
		m.blocks = append(m.blocks, block{kind: bkError,
			text: "interactive login supports the openai provider only"})
		m.refresh()
		return nil
	}
	m.busy = true
	m.blocks = append(m.blocks, block{kind: bkNotice, text: "logging in to " + provider + ": opening browser..."})
	m.refresh()
	return func() tea.Msg {
		rec, err := auth.LoginChatGPT(context.Background(), false)
		if err == nil {
			err = auth.Put(provider, rec)
		}
		return loginDoneMsg{provider: provider, thenSwitch: thenSwitch, err: err}
	}
}

func (m *Model) doLogout(provider string) {
	if err := auth.Delete(provider); err != nil {
		m.blocks = append(m.blocks, block{kind: bkError, text: "logout: " + err.Error()})
	} else {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "logged out of " + provider})
	}
	m.refresh()
}

func (m Model) modelPickerView() string {
	w := m.overlayInnerWidth()
	const maxRows = 8
	mp := m.models
	start := 0
	if mp.cursor >= maxRows {
		start = mp.cursor - maxRows + 1
	}
	title := overlayTitleStyle.Render("Model  ") +
		overlayHintStyle.Render("(type to filter · ↑/↓ · Enter select · Esc close)")
	rows := []string{padLine(title, w, cSurface0)}
	if mp.query != "" {
		rows = append(rows, padLine(overlayTextStyle.Render(" /"+mp.query), w, cSurface0))
	}
	if len(mp.items) == 0 {
		rows = append(rows, padLine(overlayHintStyle.Render(" (no matches)"), w, cSurface0))
	}
	for i := start; i < len(mp.items) && i < start+maxRows; i++ {
		it := mp.items[i]
		icon := "  "
		switch {
		case it.locked:
			icon = "🔒"
		case it.current:
			icon = "● "
		}
		line := fmt.Sprintf(" %s %s", icon, truncate(it.ref, max(8, w-6)))
		if i == mp.cursor {
			rows = append(rows, pickSelStyle.Width(w).MaxWidth(w).Render(line))
		} else {
			rows = append(rows, pickRowStyle.Width(w).MaxWidth(w).Render(line))
		}
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// localWizardView renders the local-model setup overlay for the current step.
func (m Model) localWizardView() string {
	w := m.overlayInnerWidth()
	wiz := m.localWiz
	title := overlayTitleStyle.Render("Local model setup  ") +
		overlayHintStyle.Render("(Enter next · Esc cancel)")
	rows := []string{padLine(title, w, cSurface0)}
	switch wiz.step {
	case wizStepSource:
		rows = append(rows, padLine(overlayTextStyle.Render(" Model source:"), w, cSurface0))
		opts := []string{"Hugging Face download (-hf)", "Local .gguf path"}
		for i, o := range opts {
			line := "   " + o
			if i == wiz.srcCur {
				line = " > " + o
			}
			rows = append(rows, padLine(overlayTextStyle.Render(line), w, cSurface0))
		}
	case wizStepValue:
		label := " HF repo:quant:"
		if wiz.srcCur == 1 {
			label = " Path to .gguf:"
		}
		rows = append(rows, padLine(overlayTextStyle.Render(label), w, cSurface0))
		rows = append(rows, padLine(overlayTextStyle.Render(" "+wiz.value+"_"), w, cSurface0))
	case wizStepBinary:
		rows = append(rows, padLine(overlayTextStyle.Render(" llama-server binary:"), w, cSurface0))
		rows = append(rows, padLine(overlayTextStyle.Render(" "+wiz.binary+"_"), w, cSurface0))
	case wizStepConfirm:
		rows = append(rows, padLine(overlayTextStyle.Render(" Command:"), w, cSurface0))
		rows = append(rows, padLine(overlayHintStyle.Render(" "+truncate(wiz.config().CommandString(), max(8, w-2))), w, cSurface0))
		rows = append(rows, padLine(overlayHintStyle.Render(" Enter to save & start"), w, cSurface0))
		if wiz.errText != "" {
			rows = append(rows, padLine(errStyle.Render(" "+wiz.errText), w, cSurface0))
		}
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// newSession discards the current conversation and starts fresh, as if the
// agent had just been launched: the chat, plan panel, context gauge, and
// session identity are all cleared. The current session is persisted first so it
// stays resumable via /resume. The system prompt is rebuilt from the current
// on-disk project instructions (when a rebuilder is set), so edits to
// AGENTS.md/CLAUDE.md/context.md since launch take effect; the launch-time
// SessionStart hook context and connected MCP servers are not re-run.
func (m *Model) newSession() {
	m.saveSession()
	m.agent.Reset()
	if m.rebuildSystem != nil {
		m.agent.SetSystem(m.rebuildSystem())
	}
	m.blocks = nil
	m.groups = map[string]*agentRun{}
	m.toolPolicy = map[string]string{}
	m.artifacts = nil
	m.artIndex = map[string]*artifact{}
	m.diffScrollX = 0
	m.diffMaxLen = 0
	m.todos = nil
	m.todoCollapsed = false
	m.sessionID = ""
	m.sessionTitle = ""
	m.sessionStart = time.Time{}
	m.ctxTokens = 0
	m.pendingImages = nil
	m.pastingImage = false
	m.history = nil
	m.histIdx = 0
	m.histDraft = ""
	if m.hooks != nil {
		m.hooks.SetSession("", "")
	}
	m.clearSelection()
	m.resetStream()
	m.input.Reset()
	m.refresh()
	m.vp.GotoTop()
}

func (m *Model) loadSession(id string) {
	s, err := session.Load(id)
	if err != nil {
		m.blocks = append(m.blocks, block{kind: bkError, text: "load session: " + err.Error()})
		return
	}
	m.agent.SetMessages(s.Messages)
	m.agent.SetSessionID(s.ID)
	m.blocks = reconstructBlocks(s.Messages)
	m.sessionID = s.ID
	m.sessionTitle = s.Title
	m.sessionStart = s.Created
	// Restore the model the session used (best-effort): a removed model leaves the
	// current backend in place with a soft notice rather than an error.
	if s.Model != "" && s.Model != m.backend.Model().Ref() {
		if m.modelReg != nil {
			if _, _, err := m.modelReg.Resolve(s.Model); err != nil {
				m.blocks = append(m.blocks, block{kind: bkNotice,
					text: "session used model " + s.Model + " (no longer available); staying on " + m.model})
			} else {
				m.switchModel(s.Model, false) // restoring a session must not change the global default
			}
		}
	}
	m.ctxTokens = m.agent.ContextTokens()
	m.resetStream()
}

// reconstructBlocks rebuilds the visible history from a saved message list.
func reconstructBlocks(msgs []llm.Message) []block {
	var bs []block
	for _, msg := range msgs {
		switch msg.Role {
		case llm.RoleUser:
			bs = append(bs, block{kind: bkUser, text: userTextWithImages(msg.Content, len(msg.Images))})
		case llm.RoleAssistant:
			if strings.TrimSpace(msg.Content) != "" {
				bs = append(bs, block{kind: bkAssistant, text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				bs = append(bs, block{kind: bkToolCall,
					text: fmt.Sprintf("%s › %s", tc.Function.Name, formatArgs(tc.Function.Name, tc.Function.Arguments))})
			}
		case llm.RoleTool:
			bs = append(bs, block{kind: bkToolResult, text: firstLines(msg.Content, 8)})
		}
	}
	return bs
}

func (m *Model) saveSession() {
	if m.sessionID == "" {
		return
	}
	s := &session.Session{
		Meta: session.Meta{
			ID:      m.sessionID,
			Title:   m.sessionTitle,
			Created: m.sessionStart,
			Model:   m.backend.Model().Ref(),
		},
		Messages: m.agent.Messages(),
	}
	if err := session.Save(s, time.Now()); err != nil {
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "could not save session: " + err.Error()})
	}
}

func (m *Model) finishTurn(msg turnDoneMsg) {
	answer := msg.answer
	if strings.TrimSpace(answer) == "" {
		answer = m.curContent
	}
	switch {
	case errors.Is(msg.err, context.Canceled):
		if strings.TrimSpace(answer) != "" {
			m.blocks = append(m.blocks, block{kind: bkAssistant, text: answer})
		}
		m.blocks = append(m.blocks, block{kind: bkNotice, text: "interrupted"})
	case msg.err != nil:
		m.blocks = append(m.blocks, block{kind: bkError, text: msg.err.Error()})
	case strings.TrimSpace(answer) != "":
		m.blocks = append(m.blocks, block{kind: bkAssistant, text: answer})
	}
	m.busy = false
	m.cancel = nil
	m.resetStream()
	m.saveSession()
	// Persist the quota reading this turn's responses carried, so `aigem usage`
	// can report it later without spending a request to ask.
	if rep, ok := llm.UsageOf(m.backend); ok {
		_ = llm.SaveLimits(rep.UsageReport().Limits)
	}
	m.refresh()
}

// sidebarWidth is the fixed column width (content + border) of the plan sidebar.
const sidebarWidth = 34

const maxInputHeight = 4

// sidebarVisible reports whether the plan panel should be shown: a non-empty
// plan, and a terminal wide enough that the floating panel still leaves a usable
// chat width on the rows it covers.
func (m *Model) sidebarVisible() bool {
	return len(m.todos) > 0 && m.width >= 80
}

func (m *Model) layout() {
	statusH := 1
	overlayH := lipgloss.Height(m.overlay())
	if m.overlay() == "" {
		overlayH = 0
	}
	m.input.SetWidth(max(10, m.width-4))
	m.resizeInputHeight()
	inputH := m.input.Height() + 2 // textarea + rounded border
	m.vp = viewport.New(m.width, max(1, m.height-inputH-statusH-overlayH))
	m.md = newGlamour(catppuccinProse, m.width)
	m.mdCode = newGlamour(catppuccinCode, m.width)
	if m.curStableLen > 0 {
		m.curStableRender = m.renderMarkdown(m.curContent[:m.curStableLen])
	}
	m.refresh()
}

func (m *Model) resizeInputHeight() bool {
	h := inputHeight(m.input.Value(), m.input.Width())
	if h == m.input.Height() {
		return false
	}
	m.input.SetHeight(h)
	return true
}

func inputHeight(value string, width int) int {
	return min(maxInputHeight, inputRows(value, width))
}

// inputRows is the total number of visual rows the value occupies, uncapped, so
// callers can tell whether the text fits within maxInputHeight.
func inputRows(value string, width int) int {
	if width < 1 {
		width = 1
	}
	if value == "" {
		return 1
	}
	rows := 0
	for _, line := range strings.Split(value, "\n") {
		rows += wrappedRows(line, width)
	}
	return rows
}

// cursorAtInputEnd reports whether the input cursor sits at the very end of the
// text. Re-pinning the textarea's scroll to the top moves the cursor to the end,
// so it is only safe when the cursor is already there (typing or backspacing).
func cursorAtInputEnd(ta textarea.Model) bool {
	lines := strings.Split(ta.Value(), "\n")
	if ta.Line() != len(lines)-1 {
		return false
	}
	li := ta.LineInfo()
	return li.StartColumn+li.ColumnOffset >= len([]rune(lines[len(lines)-1]))
}

// wrappedRows mirrors the bubbles textarea's internal word-wrap so the input box
// is sized to the exact number of visual rows the textarea renders. A naive
// ceil(width/cols) under-counts: words are kept whole (a word that does not fit
// is pushed to the next row), and a line whose content exactly fills the width
// gains a trailing phantom row. Diverging by even one row leaves the textarea's
// viewport scrolled with the first line clipped out of view.
func wrappedRows(line string, width int) int {
	rows := 1
	var cur, word, lastRune, spaces int
	for _, r := range line {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			rw := ansi.StringWidth(string(r))
			word += rw
			lastRune = rw
		}
		switch {
		case spaces > 0:
			if cur+word+spaces > width {
				rows++
				cur = word + spaces
			} else {
				cur += word + spaces
			}
			word, spaces = 0, 0
		case word+lastRune > width:
			if cur > 0 {
				rows++
				cur = 0
			}
			cur += word
			word = 0
		}
	}
	if cur+word+spaces >= width {
		rows++
	}
	return rows
}

// renderMarkdown renders s as markdown. Prose and fenced code are rendered by
// separate glamour renderers (card vs surface0 background); code segments are
// then padded to full width so the panel is solid rather than ragged - glamour
// never pads code lines itself.
func (m *Model) renderMarkdown(s string) string {
	cw := m.width
	if m.md == nil {
		return assistantStyle.Width(cw).Render(s)
	}
	// Expand tabs up front: lipgloss.Width counts a tab as one cell but terminals
	// render it wider, which would desync padCodePanel and leave ragged panels.
	s = strings.ReplaceAll(s, "\t", "    ")
	var parts []string
	for _, seg := range splitCodeBlocks(s) {
		if seg.code && m.mdCode != nil {
			out, err := m.mdCode.Render(seg.text)
			if err != nil {
				out = assistantStyle.Render(seg.text)
			}
			if out = trimBlankLines(out); out != "" {
				parts = append(parts, padCodePanel(out, max(20, cw-4)))
			}
			continue
		}
		// Prose may embed tables; render each table run through the code renderer
		// (so its cells sit on the same dark well as code) and seal it into a solid
		// panel - glamour emits table borders and padding with no background, which
		// would otherwise punch the terminal default through the panel.
		for _, sub := range splitTables(seg.text) {
			r := m.md
			if sub.table && m.mdCode != nil {
				r = m.mdCode
			}
			out, err := r.Render(sub.text)
			if err != nil {
				out = assistantStyle.Render(sub.text)
			}
			if out = trimBlankLines(out); out == "" {
				continue
			}
			if sub.table {
				out = solidPanel(out, max(20, cw-4), cCodeBg)
			}
			parts = append(parts, out)
		}
	}
	// Each segment is trimmed of its boundary blank lines, so one blank line
	// between segments gives even spacing regardless of glamour's own margins.
	return strings.Join(parts, "\n\n")
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// trimBlankLines drops leading and trailing lines whose only visible content is
// whitespace (ignoring ANSI styling), without touching blank lines inside a
// code panel.
func trimBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	blank := func(ln string) bool { return strings.TrimSpace(ansiRE.ReplaceAllString(ln, "")) == "" }
	for len(lines) > 0 && blank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// padCodePanel right-pads every line of a rendered code block to width w with the
// code background, turning glamour's token-only highlighting into a solid panel
// with no ragged trailing gap.
func padCodePanel(s string, w int) string {
	fill := lipgloss.NewStyle().Background(cCodeBg)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if gap := w - lipgloss.Width(ln); gap > 0 {
			lines[i] = ln + fill.Render(strings.Repeat(" ", gap))
		}
	}
	return strings.Join(lines, "\n")
}

// solidPanel seals a rendered block into a gap-free rectangle on bg: it pads every
// line to width w and re-asserts bg at the start of the line and after each SGR
// reset, so unstyled regions (glamour's table borders and column padding, which
// carry no background) show the panel color instead of the terminal default.
func solidPanel(s string, w int, bg lipgloss.Color) string {
	seq := colorProfile.Color(string(bg)).Sequence(true)
	if seq == "" { // no-color profile (e.g. tests): nothing to re-assert
		return s
	}
	open := "\x1b[" + seq + "m"
	fill := lipgloss.NewStyle().Background(bg)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		ln = open + strings.ReplaceAll(ln, "\x1b[0m", "\x1b[0m"+open)
		if gap := w - lipgloss.Width(ln); gap > 0 {
			ln += fill.Render(strings.Repeat(" ", gap))
		}
		lines[i] = ln
	}
	return strings.Join(lines, "\n")
}

// proseSegment is a run of prose markdown that is either an embedded table (a
// header row plus its delimiter row and body) or everything else.
type proseSegment struct {
	text  string
	table bool
}

// tableDelimRE matches a GitHub-flavored table delimiter row, e.g. "|---|:--:|".
var tableDelimRE = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)

// splitTables splits prose into ordered table and non-table runs. A table is a
// line containing a pipe immediately followed by a delimiter row that also
// contains a pipe; it extends through the following pipe-bearing body rows.
func splitTables(s string) []proseSegment {
	pipe := func(l string) bool { return strings.Contains(l, "|") }
	lines := strings.Split(s, "\n")
	var segs []proseSegment
	var buf []string
	flush := func(table bool) {
		if len(buf) > 0 {
			segs = append(segs, proseSegment{strings.Join(buf, "\n"), table})
			buf = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		if pipe(lines[i]) && i+1 < len(lines) && pipe(lines[i+1]) && tableDelimRE.MatchString(lines[i+1]) {
			flush(false)
			buf = append(buf, lines[i], lines[i+1])
			i += 2
			for ; i < len(lines) && pipe(lines[i]) && strings.TrimSpace(lines[i]) != ""; i++ {
				buf = append(buf, lines[i])
			}
			i--
			flush(true)
			continue
		}
		buf = append(buf, lines[i])
	}
	flush(false)
	return segs
}

// codeSegment is a run of markdown that is either prose or a single fenced code
// block (fences included).
type codeSegment struct {
	text string
	code bool
}

// splitCodeBlocks splits s into ordered prose and fenced-code segments, so each
// can be rendered on its own background. A trailing unclosed fence is treated as
// code.
func splitCodeBlocks(s string) []codeSegment {
	var segs []codeSegment
	var buf []string
	inCode := false
	flush := func() {
		if len(buf) > 0 {
			segs = append(segs, codeSegment{text: strings.Join(buf, "\n"), code: inCode})
			buf = nil
		}
	}
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			if !inCode {
				flush() // close prose, open code with this fence
				inCode = true
				buf = append(buf, ln)
			} else {
				buf = append(buf, ln) // close code with this fence
				flush()
				inCode = false
			}
			continue
		}
		buf = append(buf, ln)
	}
	flush()
	return segs
}

// resetStream clears the per-turn streaming buffers and the progressive-render
// cache so the next turn starts clean.
func (m *Model) resetStream() {
	m.curContent, m.curReason = "", ""
	m.curStableLen, m.curStableRender = 0, ""
}

// updateStable advances the stable/tail split as new content streams in and
// re-renders the stable prefix through glamour only when it grows, so closed
// markdown blocks get highlighted live while the open tail stays plain text.
func (m *Model) updateStable() {
	if n := stableSplit(m.curContent); n != m.curStableLen {
		m.curStableLen = n
		m.curStableRender = m.renderMarkdown(m.curContent[:n])
	}
}

// stableSplit returns the byte offset up to which s forms complete markdown
// blocks safe to render: just past the last blank line that is not inside an
// open code fence. Content after it is an unfinished block still streaming.
func stableSplit(s string) int {
	inFence, idx, lineStart := false, 0, 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '\n' {
			continue
		}
		trimmed := strings.TrimSpace(s[lineStart:i])
		switch {
		case strings.HasPrefix(trimmed, "```"), strings.HasPrefix(trimmed, "~~~"):
			inFence = !inFence
		case i < len(s) && !inFence && trimmed == "" && lineStart > 0:
			idx = i + 1
		}
		lineStart = i + 1
	}
	return idx
}

// fillWidth pads every line to width w with cBase. Short lines - user echoes,
// tool lines, lists, anything not already stretched edge to edge - would
// otherwise be padded to width by the viewport with no background, punching the
// canvas color out at the right. Pre-filling to full width also keeps the
// viewport from re-wrapping (and dropping the background of) those lines.
func fillWidth(s string, w int) string {
	fill := lipgloss.NewStyle().Background(cBase)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if gap := w - lipgloss.Width(ln); gap > 0 {
			lines[i] = ln + fill.Render(strings.Repeat(" ", gap))
		}
	}
	return strings.Join(lines, "\n")
}

// refresh recomputes the viewport content from history plus the live turn.
func (m *Model) refresh() {
	if !m.ready {
		return
	}
	if len(m.blocks) == 0 && m.curContent == "" && !m.busy {
		m.vp.SetContent(lipgloss.Place(
			m.vp.Width, m.vp.Height, lipgloss.Center, lipgloss.Center, m.welcome(),
			lipgloss.WithWhitespaceBackground(cBase)))
		return
	}

	var b strings.Builder
	b.WriteString(m.renderHistory())
	if m.curContent != "" {
		var seg strings.Builder
		seg.WriteString(m.curStableRender)
		if tail := m.curContent[m.curStableLen:]; strings.TrimSpace(tail) != "" {
			if seg.Len() > 0 {
				seg.WriteString("\n")
			}
			seg.WriteString(assistantStyle.Width(max(20, m.width-4)).Render(tail))
		}
		b.WriteString("\n" + liveGutterStyle.Render(seg.String()))
	}
	if m.busy && m.pending == nil && m.picker == nil && m.curContent == "" && !m.hasRunningGroup() {
		thinking := lipgloss.NewStyle().Foreground(cMauve).Background(cBase).Italic(true).Render(" Thinking")
		b.WriteString("\n" + m.spin.View() + thinking)
	}
	// Stay pinned to the bottom only when already there; if the user scrolled up
	// to read history, keep their position as new content streams in.
	following := m.vp.AtBottom()
	m.vpContent = fillWidth(b.String(), m.vp.Width)
	m.vp.SetContent(m.vpContent)
	if following {
		m.vp.GotoBottom()
	}
}

// renderHistory renders the persisted blocks, honoring the tool-output toggle.
// A bkAgentGroup expands to its subagent run's header plus that run's nested
// lines, so concurrent delegations stay grouped and attributed.
func (m *Model) renderHistory() string {
	var b strings.Builder
	for _, blk := range m.blocks {
		if blk.kind == bkAgentGroup {
			g := m.groups[blk.ref]
			if g == nil {
				continue
			}
			b.WriteString("\n" + m.renderAgentHeader(g) + "\n")
			for _, ln := range g.lines {
				b.WriteString(m.renderLine(ln))
			}
			continue
		}
		b.WriteString(m.renderLine(blk))
	}
	return b.String()
}

// renderLine renders one non-group block, honoring the tool-output toggle and
// the block's nesting depth.
func (m *Model) renderLine(blk block) string {
	pad := strings.Repeat("  ", blk.depth)
	switch blk.kind {
	case bkUser:
		return "\n" + m.renderUserText(blk.text) + "\n"
	case bkToolCall:
		return toolStyle.Render(pad+"· "+blk.text) + "\n"
	case bkToolResult:
		if m.showToolOutput {
			return resultStyle.Render(indent(blk.text, pad+"  ⤷ ")) + "\n"
		}
	case bkToolError:
		// Errors are always shown so a failed tool never looks like a success.
		return errStyle.Render(indent(blk.text, pad+"  ⤷ ")) + "\n"
	case bkAssistant:
		return "\n" + gutterStyle.Render(m.renderMarkdown(blk.text)) + "\n"
	case bkNotice:
		return noticeStyle.Render(pad+"  ⚠ "+blk.text) + "\n"
	case bkError:
		return "\n" + errStyle.Render("[error] "+blk.text) + "\n"
	case bkSkill:
		return "\n" + skillLineStyle.Render("◆ skill: "+blk.text) + "\n"
	case bkMcpPrompt:
		return "\n" + skillLineStyle.Render("◆ mcp: "+blk.text) + "\n"
	case bkDiff:
		return "\n" + m.renderDiff(blk.text, blk.diffOps) + "\n"
	case bkDownload:
		// Text is pre-rendered (name + progress bar + stats); emit it as-is so the
		// bar's gradient colors are not overwritten by a wrapping style.
		return "\n" + blk.text + "\n"
	}
	return ""
}

func (m *Model) renderUserText(text string) string {
	contentW := max(1, m.width-2)
	var rows []string
	for lineIdx, line := range strings.Split(text, "\n") {
		wrapped := ansi.Wordwrap(line, contentW, "")
		for wrapIdx, part := range strings.Split(wrapped, "\n") {
			prefix := "  "
			if lineIdx == 0 && wrapIdx == 0 {
				prefix = "› "
			}
			rows = append(rows, userStyle.Render(prefix+part))
		}
	}
	return strings.Join(rows, "\n")
}

// renderAgentHeader renders a subagent run's status line: a spinner while it
// runs, then a green check or red cross, followed by its name and prompt.
func (m *Model) renderAgentHeader(g *agentRun) string {
	var icon string
	switch {
	case !g.done:
		icon = m.spin.View()
	case g.failed:
		icon = errStyle.Render("✗")
	default:
		icon = lipgloss.NewStyle().Foreground(cGreen).Background(cBase).Render("✓")
	}
	name := lipgloss.NewStyle().Foreground(cPeach).Background(cBase).Bold(true).Render("▸ " + g.name)
	return icon + " " + name + resultStyle.Render("  "+oneLine(g.prompt))
}

// welcome is the centered banner shown before the first message.
func (m Model) welcome() string {
	// Block "aigem" logo, one Catppuccin hue per letter (mauve→blue→sapphire→teal→green).
	letter := func(c lipgloss.Color, rows ...string) string {
		st := lipgloss.NewStyle().Foreground(c).Background(cBase)
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = st.Render(r)
		}
		return lipgloss.JoinVertical(lipgloss.Left, out...)
	}
	sp := letter(cBase, " ", " ", " ", " ", " ")
	logo := lipgloss.JoinHorizontal(lipgloss.Top,
		letter(cMauve, " ███ ", "█   █", "█████", "█   █", "█   █"),
		sp,
		letter(cBlue, "█████", "  █  ", "  █  ", "  █  ", "█████"),
		sp,
		letter(cSapphire, " ████", "█    ", "█  ██", "█   █", " ████"),
		sp,
		letter(cTeal, "█████", "█    ", "████ ", "█    ", "█████"),
		sp,
		letter(cGreen, "█   █", "██ ██", "█ █ █", "█   █", "█   █"),
	)

	hint := lipgloss.NewStyle().Foreground(cSubtext0).Background(cBase)
	faint := lipgloss.NewStyle().Foreground(cOverlay0).Background(cBase)
	lines := []string{
		logo,
		"",
		hint.Render("a minimal local coding agent"),
		"",
		faint.Render("Ask it to read, search, or edit files - type below to begin."),
		faint.Render("/resume reopen a session · Ctrl+O tool output · scroll: wheel / PgUp / PgDn"),
	}
	// Center each line over a cBase-backed pad. JoinVertical(Center) would pad the
	// narrower lines with unstyled cells (the terminal's default bg, lighter than
	// cBase), drawing a visible box around the banner; PlaceHorizontal paints the
	// pad with cBase so the banner blends into the canvas.
	w := 0
	for _, l := range lines {
		w = max(w, lipgloss.Width(l))
	}
	for i, l := range lines {
		lines[i] = lipgloss.PlaceHorizontal(w, lipgloss.Center, l, lipgloss.WithWhitespaceBackground(cBase))
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// overlay returns the confirm box or resume picker shown above the input, or "".
func (m Model) overlay() string {
	switch {
	case m.trustAsk:
		return m.trustView()
	case m.skillAsk != nil:
		return m.skillTrustView()
	case m.pending != nil:
		return m.confirmView()
	case m.alert != nil:
		return m.alertView()
	case m.picker != nil:
		return m.pickerView()
	case m.browser != nil:
		return m.skillBrowserView()
	case m.agentBr != nil:
		return m.agentBrowserView()
	case m.mcp != nil:
		return m.mcpBrowserView()
	case m.localWiz != nil:
		return m.localWizardView()
	case m.localChoice != nil:
		return m.localChoiceView()
	case m.models != nil:
		return m.modelPickerView()
	case m.artBr != nil:
		return m.artifactBrowserView()
	case m.cmdMenu != nil:
		return m.cmdMenuView()
	case m.fileMenu != nil:
		return m.fileMenuView()
	}
	return ""
}

// padLine pads a single (possibly pre-styled) line to width w, filling the
// trailing space with bg. Single-line width padding is reliable, unlike a
// block Width over JoinVertical'd content.
func padLine(s string, w int, bg lipgloss.Color) string {
	return lipgloss.NewStyle().Background(bg).Width(w).MaxWidth(w).Render(s)
}

// trailFillRe matches the textarea's trailing filler (plain spaces and resets),
// which it emits unstyled in the placeholder state, leaving an unfilled gap.
var trailFillRe = regexp.MustCompile(`(?:\x1b\[0m|\s)+$`)

// fillInput repaints the input view edge to edge with cBase so the field has no
// unfilled gap (the textarea does not style its placeholder padding).
func fillInput(view string, w int) string {
	lines := strings.Split(trailFillRe.ReplaceAllString(view, ""), "\n")
	for i, line := range lines {
		lines[i] = padLine(line, w, cBase)
	}
	return strings.Join(lines, "\n")
}

// overlayInnerWidth is the content width inside an overlay box (minus border
// and horizontal padding), so each line fills the box edge to edge.
func (m Model) overlayInnerWidth() int { return max(16, m.width-4) }

func (m Model) confirmView() string {
	w := m.overlayInnerWidth()
	nameStyle := lipgloss.NewStyle().Foreground(cGreen).Background(cSurface0).Bold(true)
	title := "Run this tool?"
	var head string
	if m.pending.pathResp != nil {
		verb := "read"
		if m.pending.write {
			verb = "modify"
		}
		title = "Let " + m.pending.name + " " + verb + " a file outside the working directory?"
		head = nameStyle.Render(truncate(m.pending.path, max(5, w-2)))
	} else {
		name := m.pending.name
		args := truncate(formatArgs(name, m.pending.args), max(5, w-len([]rune(name))-4))
		head = nameStyle.Render(name) + overlayTextStyle.Render("  "+args)
	}
	labels := m.confirmOptions()
	opts := make([]string, len(labels))
	for i, l := range labels {
		if confirmChoice(i) == m.confirmIdx {
			opts[i] = optSelStyle.Render(l)
		} else {
			opts[i] = optStyle.Render(l)
		}
	}
	hint := "←/→ select · Enter confirm · Esc " + strings.ToLower(labels[len(labels)-1])
	body := lipgloss.JoinVertical(lipgloss.Left,
		padLine(overlayTitleStyle.Render(truncate(title, w)), w, cSurface0),
		padLine(head, w, cSurface0),
		padLine(strings.Join(opts, overlayTextStyle.Render(" ")), w, cSurface0),
		padLine(overlayHintStyle.Render(hint), w, cSurface0),
	)
	return overlayBoxStyle.Render(body)
}

func (m Model) pickerView() string {
	w := m.overlayInnerWidth()
	const maxRows = 8
	p := m.picker
	start := 0
	if p.cursor >= maxRows {
		start = p.cursor - maxRows + 1
	}
	titleW := max(10, w-22)
	title := overlayTitleStyle.Render("Resume a session  ") +
		overlayHintStyle.Render("(↑/↓ · Enter open · Esc cancel)")
	rows := []string{padLine(title, w, cSurface0)}
	for i := start; i < len(p.items) && i < start+maxRows; i++ {
		it := p.items[i]
		line := fmt.Sprintf(" %-*s  %s", titleW, truncate(it.Title, titleW),
			it.Updated.Local().Format("2006-01-02 15:04"))
		if i == p.cursor {
			rows = append(rows, pickSelStyle.Width(w).MaxWidth(w).Render(line))
		} else {
			rows = append(rows, pickRowStyle.Width(w).MaxWidth(w).Render(line))
		}
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m Model) skillBrowserView() string {
	w := m.overlayInnerWidth()
	b := m.browser
	if b.detail {
		return m.skillDetailView(b.items[b.cursor], w)
	}
	const maxRows = 8
	start := 0
	if b.cursor >= maxRows {
		start = b.cursor - maxRows + 1
	}
	nameW := max(8, w/3)
	title := overlayTitleStyle.Render("Skills  ") +
		overlayHintStyle.Render(fmt.Sprintf("(%d · ↑/↓ · Enter view · Esc close)", len(b.items)))
	rows := []string{padLine(title, w, cSurface0)}
	for i := start; i < len(b.items) && i < start+maxRows; i++ {
		s := b.items[i]
		line := fmt.Sprintf(" %-*s  %s", nameW, truncate(s.Name, nameW),
			truncate(oneLine(s.Description), max(8, w-nameW-4)))
		if i == b.cursor {
			rows = append(rows, pickSelStyle.Width(w).MaxWidth(w).Render(line))
		} else {
			rows = append(rows, pickRowStyle.Width(w).MaxWidth(w).Render(line))
		}
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m Model) skillDetailView(s *skill.Skill, w int) string {
	var rows []string
	add := func(st lipgloss.Style, text string) {
		rows = append(rows, padLine(st.Render(text), w, cSurface0))
	}
	add(overlayTitleStyle, "◆ "+s.Name)
	if s.ArgHint != "" {
		add(overlayHintStyle, "args: "+s.ArgHint)
	}
	for _, l := range wrapText(s.Description, w) {
		add(overlayTextStyle, l)
	}
	for _, l := range firstNLines(s.Body(), 5) {
		add(resultStyle.Foreground(cOverlay0).Background(cSurface0), "  "+truncate(l, w-2))
	}
	add(overlayHintStyle, s.Path)
	add(overlayHintStyle, "Enter run · Esc back")
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m Model) mcpBrowserView() string {
	w := m.overlayInnerWidth()
	b := m.mcp
	if b.loading || b.preview != "" {
		return m.mcpPreviewView(w)
	}
	var rows []string
	add := func(st lipgloss.Style, text string) {
		rows = append(rows, padLine(st.Render(text), w, cSurface0))
	}
	add(overlayTitleStyle, "MCP servers  "+overlayHintStyle.Render("(↑/↓ · Enter preview · Esc close)"))
	for _, sv := range b.servers {
		if sv.Connected {
			line := fmt.Sprintf("● %s  tools:%d prompts:%d resources:%d",
				sv.Name, len(sv.Tools), len(sv.Prompts), len(sv.Resources))
			add(overlayTextStyle, truncate(line, w))
		} else {
			add(overlayHintStyle, truncate("○ "+sv.Name+"  failed: "+sv.Err, w))
		}
	}
	if len(b.items) == 0 {
		add(overlayHintStyle, "(no readable resources)")
		return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
	}
	add(overlayHintStyle, "Resources:")
	const maxRows = 6
	start := 0
	if b.cursor >= maxRows {
		start = b.cursor - maxRows + 1
	}
	for i := start; i < len(b.items) && i < start+maxRows; i++ {
		it := b.items[i]
		label := it.uri
		if it.name != "" {
			label = it.name + "  " + it.uri
		}
		line := " " + truncate(label, w-2)
		if i == b.cursor {
			rows = append(rows, pickSelStyle.Width(w).MaxWidth(w).Render(line))
		} else {
			rows = append(rows, pickRowStyle.Width(w).MaxWidth(w).Render(line))
		}
	}
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m Model) mcpPreviewView(w int) string {
	b := m.mcp
	var rows []string
	add := func(st lipgloss.Style, text string) {
		rows = append(rows, padLine(st.Render(text), w, cSurface0))
	}
	it := b.items[b.cursor]
	add(overlayTitleStyle, "◆ "+truncate(it.uri, w-2))
	if b.loading {
		add(overlayTextStyle, "loading...")
		return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
	}
	for _, l := range firstNLines(b.preview, 12) {
		add(resultStyle.Foreground(cOverlay0).Background(cSurface0), "  "+truncate(l, w-2))
	}
	add(overlayHintStyle, "Esc back")
	return overlayBoxStyle.Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// wrapText word-wraps s to width w.
func wrapText(s string, w int) []string {
	if w < 8 {
		w = 8
	}
	var lines []string
	for _, para := range strings.Split(strings.TrimSpace(s), "\n") {
		cur := ""
		for _, word := range strings.Fields(para) {
			switch {
			case cur == "":
				cur = word
			case len([]rune(cur))+1+len([]rune(word)) <= w:
				cur += " " + word
			default:
				lines = append(lines, cur)
				cur = word
			}
		}
		if cur != "" {
			lines = append(lines, cur)
		}
	}
	return lines
}

func firstNLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return lines
}

func (m Model) View() string {
	if !m.ready {
		return "starting aigem..."
	}
	chat := m.vp.View()
	if m.selActive {
		chat = m.highlightSelection(chat)
	}
	if m.sidebarVisible() {
		chat = m.overlaySidebar(chat)
	}
	parts := []string{chat}
	if ov := m.overlay(); ov != "" {
		parts = append(parts, ov)
	}
	parts = append(parts,
		inputBoxStyle.Render(fillInput(m.input.View(), max(10, m.width-4))),
		statusBarStyle.Width(m.width).Render(m.statusLine()),
	)
	return screenStyle.Width(m.width).Height(m.height).Render(strings.Join(parts, "\n"))
}

// sidebarLines builds the plan panel as a full bordered window: a slice of
// fixed-width rows (each sidebarWidth cells) framed with a rounded box. The
// height is the content height - top + title + bottom when collapsed, plus the
// items when expanded - so the window floats over only as many chat rows as it
// needs.
func (m Model) sidebarLines() []string {
	inner := sidebarWidth - 2 // body width between the two vertical borders
	textW := inner - 3        // wrap width after a leading pad and the 2-cell glyph
	border := lipgloss.NewStyle().Foreground(cMauve).Background(cCard)
	side := border.Render("│")
	rule := func(l, r string) string { return border.Render(l + strings.Repeat("─", inner) + r) }
	body := func(s string) string { return side + padLine(s, inner, cCard) + side }

	var done int
	for _, t := range m.todos {
		if t.Status == agent.TodoCompleted {
			done++
		}
	}
	chevron := "▾"
	if m.todoCollapsed {
		chevron = "▸"
	}
	title := lipgloss.NewStyle().Foreground(cMauve).Bold(true).Background(cCard).
		Render(fmt.Sprintf(" %s Plan  %d/%d", chevron, done, len(m.todos)))
	out := []string{rule("╭", "╮"), body(title)}
	if m.todoCollapsed {
		return append(out, rule("╰", "╯"))
	}
	for _, t := range m.todos {
		glyph, fg := "○", cSubtext0
		switch t.Status {
		case agent.TodoCompleted:
			glyph, fg = "✓", cOverlay1
		case agent.TodoInProgress:
			glyph, fg = "▶", cYellow
		}
		st := lipgloss.NewStyle().Foreground(fg).Background(cCard)
		wrapped := strings.Split(lipgloss.NewStyle().Width(textW).Render(t.Text), "\n")
		for i, wl := range wrapped {
			prefix := glyph + " "
			if i > 0 {
				prefix = "  "
			}
			out = append(out, body(" "+st.Render(prefix+strings.TrimRight(ansiRE.ReplaceAllString(wl, ""), " "))))
		}
	}
	return append(out, rule("╰", "╯"))
}

// overlaySidebar composites the plan panel over the top-right of the rendered
// chat: each panel row replaces the rightmost sidebarWidth cells of the matching
// chat line (ANSI-aware), leaving rows below the panel at full chat width.
func (m Model) overlaySidebar(chat string) string {
	side := m.sidebarLines()
	leftW := m.width - sidebarWidth
	gap := lipgloss.NewStyle().Background(cBase)
	lines := strings.Split(chat, "\n")
	for i, s := range side {
		if i >= len(lines) {
			break
		}
		left := ansi.Truncate(lines[i], leftW, "")
		if fill := leftW - ansi.StringWidth(left); fill > 0 {
			left += gap.Render(strings.Repeat(" ", fill))
		}
		lines[i] = left + s
	}
	return strings.Join(lines, "\n")
}

// quotaSegment renders the tightest quota window the provider reported on its
// last response - the one that will cut the session off first - plus how long
// until it resets. It is empty until a response has actually carried quota
// headers, and stays empty for providers that send none (the local model): there
// is no endpoint to poll for this.
func (m Model) quotaSegment() (string, float64) {
	rep, ok := llm.UsageOf(m.backend)
	if !ok {
		return "", 0
	}
	w, ok := rep.UsageReport().Limits.Tightest()
	if !ok {
		return "", 0
	}
	s := "quota " + w.Remaining + " left"
	if w.UsedPercent > 0 {
		s = fmt.Sprintf("quota %g%%", math.Round(w.UsedPercent*10)/10)
	}
	if d := time.Until(w.ResetAt); !w.ResetAt.IsZero() && d > 0 {
		s += " " + llm.FormatDuration(d)
	}
	return s, w.UsedPercent
}

// statusLine builds the colored bottom bar: model, server, context gauge, state, tools.
func (m Model) statusLine() string {
	seg := func(c lipgloss.Color, s string) string {
		return lipgloss.NewStyle().Foreground(c).Background(cMantle).Render(s)
	}
	sep := seg(cOverlay0, " · ")

	pct := 0
	if m.ctxSize > 0 {
		pct = m.ctxTokens * 100 / m.ctxSize
	}
	ctxColor := cTeal
	switch {
	case pct >= 80:
		ctxColor = cRed
	case pct >= 50:
		ctxColor = cPeach
	}
	ctx := seg(ctxColor, fmt.Sprintf("ctx %s/%s %d%%", humanTokens(m.ctxTokens), humanTokens(m.ctxSize), pct))
	if q, qpct := m.quotaSegment(); q != "" {
		qColor := cTeal
		switch {
		case qpct >= 90:
			qColor = cRed
		case qpct >= 70:
			qColor = cPeach
		}
		ctx += sep + seg(qColor, q)
	}

	state := seg(cGreen, "ready")
	if m.busy {
		state = seg(cYellow, "working (Esc to stop)")
	}
	tools := "tools hidden"
	if m.showToolOutput {
		tools = "tools shown"
	}
	// Auth dot: green when the active provider is usable, red when it needs login.
	authColor := cGreen
	if provID := m.backend.Model().Provider; m.modelReg != nil {
		if p, ok := m.modelReg.Provider(provID); ok && p.NeedsAuth() && !auth.IsAuthenticated(provID) {
			authColor = cRed
		}
	} else if provID := m.backend.Model().Provider; !auth.IsAuthenticated(provID) {
		authColor = cRed
	}
	dot := seg(authColor, "●")
	line := dot + seg(cMauve, " "+m.model) + sep + ctx + sep + state +
		sep + seg(cOverlay1, "^O "+tools)
	if m.pastingImage {
		line += sep + seg(cYellow, "pasting image")
	}
	if n := len(m.pendingImages); n > 0 {
		line += sep + seg(cSapphire, "^V "+imagesLabel(n)+" attached")
	}
	if m.autoMode {
		line += sep + seg(cPeach, "⇧⇥ auto")
	}
	if n := m.mcpConnected(); n > 0 {
		line += sep + seg(cTeal, fmt.Sprintf("mcp:%d", n))
	}
	if n := len(m.artifacts); n > 0 {
		line += sep + seg(cPeach, fmt.Sprintf("%d artifact%s · /artifacts", n, plural(n)))
	}
	if len(m.todos) > 0 {
		plan := "plan"
		if m.todoCollapsed {
			plan = "plan ▸"
		}
		line += sep + seg(cOverlay1, "^T "+plan)
	}
	// When scrolled up, show how far and how to follow again.
	if m.ready && !m.vp.AtBottom() {
		line += sep + seg(cPeach, fmt.Sprintf("↑ %d%% (PgDn to follow)", int(m.vp.ScrollPercent()*100)))
	}
	return line
}

// mcpConnected counts connected MCP servers for the status bar.
func (m Model) mcpConnected() int {
	n := 0
	for _, sv := range m.mcpMgr.Servers() {
		if sv.Connected {
			n++
		}
	}
	return n
}

// ---- helpers ----

func humanTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

func truncate(s string, n int) string {
	if n < 1 {
		n = 1
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// retryAttempts is how many total tries an interactive LLM call gets before the
// error reaches the user. Someone is waiting, so this rides out a brief provider
// hiccup without turning a failure into a long silence.
const retryAttempts = 3

// formatRetry renders a pending retry as a one-line notice. The provider's error
// body is long and JSON, so only its first line is shown, trimmed.
func formatRetry(n llm.RetryNotice) string {
	reason := ""
	if n.Err != nil {
		reason = ": " + truncate(strings.TrimSpace(firstLines(n.Err.Error(), 1)), 100)
	}
	return fmt.Sprintf("provider call failed, retrying in %s (attempt %d of %d)%s",
		n.Delay.Round(time.Second), n.Attempt+1, n.Attempts, reason)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + "\n..."
}

// formatArgs renders a tool's JSON arguments compactly. Bulky text fields
// (file contents, replacement strings) collapse to a line count so the call
// line stays readable.
func formatArgs(tool, raw string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || len(m) == 0 {
		return oneLine(strings.TrimSpace(raw))
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := argValue(k, m[k])
		if len(keys) == 1 {
			parts = append(parts, v)
		} else {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "  ")
}

// bulkArgKeys are arguments whose value is file-sized text we summarize as a
// line count instead of dumping into the call line.
var bulkArgKeys = map[string]bool{
	"content": true, "old_string": true, "new_string": true,
}

func argValue(key string, v any) string {
	s := fmt.Sprint(v)
	if bulkArgKeys[key] || strings.Contains(s, "\n") {
		return fmt.Sprintf("%d lines", strings.Count(s, "\n")+1)
	}
	return oneLine(s)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 100 {
		s = string([]rune(s)[:100]) + "…"
	}
	return s
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// Run starts the Bubble Tea program.
func Run(m Model) error {
	lipgloss.SetColorProfile(colorProfile)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}
