// Package hooks runs user/project/skill-defined shell hooks at agent lifecycle
// points, compatible with Claude Code's hook protocol (settings.json shape,
// stdin/stdout JSON, exit-code semantics). Hooks can observe, block, modify a
// tool's input or output, and inject context.
package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/trust"
)

// Event names fired by the agent loop and front-end.
const (
	EventPreToolUse       = "PreToolUse"
	EventPostToolUse      = "PostToolUse"
	EventUserPromptSubmit = "UserPromptSubmit"
	EventStop             = "Stop"
	EventSubagentStop     = "SubagentStop"
	EventSessionStart     = "SessionStart"
	EventSessionEnd       = "SessionEnd"
	EventNotification     = "Notification"
	// EventPreCompact fires before stage-3 summarization; its matcher selects on
	// the trigger ("manual" or "auto").
	EventPreCompact = "PreCompact"
)

// Hook is a single handler. v1 supports the "command" type only.
type Hook struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Shell   string   `json:"shell"`
	Timeout int      `json:"timeout"` // seconds; default 60
}

// Matcher binds a set of hooks to a tool-name pattern.
type Matcher struct {
	Matcher string `json:"matcher"` // "" / "*" = all; "A|B" any; else regexp
	Hooks   []Hook `json:"hooks"`
}

// Config mirrors the hooks section of a settings.json file.
type Config struct {
	Hooks           map[string][]Matcher `json:"hooks"`
	DisableAllHooks bool                 `json:"disableAllHooks"`
}

// Input is the JSON written to a hook command's stdin. Common fields are filled
// by the runner; the caller supplies the event-specific ones.
type Input struct {
	SessionID      string          `json:"session_id,omitempty"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	Cwd            string          `json:"cwd,omitempty"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   string          `json:"tool_response,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Source         string          `json:"source,omitempty"`
	AgentType      string          `json:"agent_type,omitempty"`
	Message        string          `json:"message,omitempty"`
}

// Decision is the aggregated outcome of every matching hook for an event.
type Decision struct {
	Block         bool            // deny / decision:"block" / exit 2
	Reason        string          // why blocked (fed back to the model)
	Allow         bool            // permissionDecision:"allow" -> skip confirm
	Ask           bool            // permissionDecision:"ask" -> force confirm
	UpdatedInput  json.RawMessage // PreToolUse replacement tool input
	UpdatedOutput *string         // PostToolUse replacement tool output
	Context       string          // concatenated additionalContext
	Continue      bool            // false => stop the whole turn
	StopReason    string          // shown when Continue is false
	SystemMessage string          // hook-supplied notice for the user
	SessionTitle  string          // SessionStart-supplied session title
	Notices       []string        // non-blocking stderr warnings
}

// Runner holds the merged, read-only hook configuration. Turn-scoped (skill)
// hooks are NOT stored here: they live on the agent and are passed to RunScoped,
// so a skill activated in one (sub)agent never leaks into another.
//
// Global hooks (from ~/.config/aigem and ~/.claude) always run. Project-local
// hooks (a repo's .aigem/.claude settings) run only once the project dir is
// trusted, so opening an untrusted repository cannot silently execute its hooks.
type Runner struct {
	mu              sync.RWMutex
	base            map[string][]Matcher // global settings (always run)
	project         map[string][]Matcher // project-local settings (gated on trust)
	disabled        bool                 // global/user disableAllHooks
	projectDisabled bool                 // project-local disableAllHooks; scoped to project hooks
	debug           io.Writer            // non-nil to log each hook's event/command/exit/decision

	hasProject  bool
	trusted     bool
	fingerprint string

	dir            string // project dir; exported to hooks as CLAUDE_PROJECT_DIR
	cwd            string
	sessionID      string
	transcriptPath string
}

// Load merges hooks from every existing settings source under cwd, returning the
// runner plus non-fatal warnings (malformed files, unsupported hook entries). A
// nil runner is never returned; an empty runner simply runs nothing.
func Load(cwd string) (*Runner, []error) {
	r := &Runner{base: map[string][]Matcher{}, project: map[string][]Matcher{}, cwd: cwd}
	r.dir = config.ProjectDir(cwd)
	if os.Getenv("AIGEM_HOOKS_DEBUG") != "" {
		r.debug = os.Stderr
	}
	var warns []error
	prefix := r.dir + string(os.PathSeparator)
	for _, path := range config.SettingsFiles(cwd) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			warns = append(warns, fmt.Errorf("%s: %w", path, err))
			continue
		}
		dst := r.base
		projectLocal := strings.HasPrefix(path, prefix)
		if projectLocal { // project-local source
			dst = r.project
		}
		if cfg.DisableAllHooks {
			if projectLocal {
				r.projectDisabled = true
			} else {
				r.disabled = true
			}
		}
		for event, matchers := range cfg.Hooks {
			warns = append(warns, validate(path, event, matchers)...)
			dst[event] = append(dst[event], matchers...)
		}
	}
	for _, ms := range r.project {
		if len(ms) > 0 {
			r.hasProject = true
		}
	}
	if r.hasProject {
		fingerprint, err := hookFingerprint(r.project, r.projectDisabled)
		if err != nil {
			warns = append(warns, fmt.Errorf("fingerprint project hooks: %w", err))
		} else {
			r.fingerprint = fingerprint
			status, err := hookTrustStatus(r.dir, fingerprint)
			if err != nil {
				warns = append(warns, err)
			} else {
				r.trusted = status.State == trust.StateAllowed
			}
		}
	} else if err := trust.MigrateLegacy(r.dir, trust.CapabilityHooks, nil); err != nil {
		warns = append(warns, fmt.Errorf("migrate legacy project hook trust: %w", err))
	}
	return r, warns
}

// HasUntrustedProjectHooks reports whether the project defines hooks that are
// withheld because the project dir is not yet trusted (the front-end should
// prompt the user).
func (r *Runner) HasUntrustedProjectHooks() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hasProject && !r.trusted
}

// ProjectDir returns the directory whose project-local hooks require trust.
func (r *Runner) ProjectDir() string { return r.dir }

// TrustProject persists the project dir as trusted and enables its hooks for
// this session.
func (r *Runner) TrustProject() error {
	r.mu.RLock()
	dir, fingerprint := r.dir, r.fingerprint
	r.mu.RUnlock()
	if fingerprint == "" {
		return fmt.Errorf("project has no hook configuration to approve")
	}
	if err := approveHooks(dir, fingerprint); err != nil {
		return err
	}
	r.mu.Lock()
	r.trusted = true
	r.mu.Unlock()
	return nil
}

// validate reports non-fatal problems with hook entries (so authors learn why a
// hook is skipped at run time instead of guessing).
func validate(path, event string, matchers []Matcher) []error {
	var warns []error
	for _, m := range matchers {
		if m.Matcher != "" && m.Matcher != "*" {
			if _, err := regexp.Compile("^(?:" + m.Matcher + ")$"); err != nil {
				warns = append(warns, fmt.Errorf("%s: %s matcher %q is not a valid regexp: %w",
					path, event, m.Matcher, err))
			}
		}
		for _, h := range m.Hooks {
			if h.Type != "" && h.Type != "command" {
				warns = append(warns, fmt.Errorf("%s: %s hook type %q is unsupported (only %q)",
					path, event, h.Type, "command"))
			}
			if strings.TrimSpace(h.Command) == "" {
				warns = append(warns, fmt.Errorf("%s: %s has a hook with an empty command", path, event))
			}
		}
	}
	return warns
}

// SetSession records the session id and transcript path included in hook input.
func (r *Runner) SetSession(id, transcriptPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionID, r.transcriptPath = id, transcriptPath
}

// matching returns the hooks for event whose matcher matches the tool name:
// global (settings) hooks, then trusted project-local hooks, then the caller's
// scoped (skill) hooks.
func (r *Runner) matching(event, toolName string, scoped map[string][]Matcher) []Hook {
	r.mu.RLock()
	disabled := r.disabled
	base := r.base[event]
	var proj []Matcher
	if r.trusted && !r.projectDisabled {
		proj = r.project[event]
	}
	r.mu.RUnlock()
	if disabled {
		return nil
	}
	var out []Hook
	collect := func(matchers []Matcher) {
		for _, m := range matchers {
			if matcherMatches(m.Matcher, toolName) {
				out = append(out, m.Hooks...)
			}
		}
	}
	collect(base)
	collect(proj)
	if scoped != nil {
		collect(scoped[event])
	}
	return out
}

var reCache sync.Map // pattern -> *regexp.Regexp (nil for an invalid pattern)

// matcherMatches reports whether pattern selects a tool name. An empty pattern
// or "*" matches all; a "|"-joined list matches any exact name; otherwise the
// pattern is compiled (and cached) as an anchored regexp.
func matcherMatches(pattern, name string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	for _, p := range strings.Split(pattern, "|") {
		if strings.TrimSpace(p) == name {
			return true
		}
	}
	re := compileMatcher(pattern)
	return re != nil && re.MatchString(name)
}

func compileMatcher(pattern string) *regexp.Regexp {
	if v, ok := reCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		re = nil
	}
	reCache.Store(pattern, re)
	return re
}

// FromAny converts a skill's parsed `hooks:` frontmatter (a generic map from
// YAML) into the runner's matcher shape via a JSON round-trip.
func FromAny(m map[string]any) (map[string][]Matcher, error) {
	if len(m) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string][]Matcher
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
