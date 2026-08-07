package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

// SkillToolName is the tool the model calls to invoke a skill.
const SkillToolName = "skill"

// skillTool lets the model invoke a discovered skill: it renders the skill's
// instructions into context (or runs it in an isolated subagent for
// context: fork) and applies its tool policy.
type skillTool struct {
	skills  *skill.Registry
	client  streamer
	tools   *tools.Registry
	temp    float64
	confirm ConfirmFunc
}

// NewSkillTool builds the skill tool. Register it into reg so the model can call
// it. Returns nil if there are no model-invocable skills.
func NewSkillTool(skills *skill.Registry, client streamer, reg *tools.Registry, temp float64,
	confirm ConfirmFunc) tools.Tool {
	if skills == nil || len(skills.ModelNames()) == 0 {
		return nil
	}
	return &skillTool{skills: skills, client: client, tools: reg, temp: temp, confirm: confirm}
}

func (t *skillTool) Name() string       { return SkillToolName }
func (t *skillTool) NeedsConfirm() bool { return false }

func (t *skillTool) Description() string {
	var b strings.Builder
	b.WriteString("Invoke a specialized skill: it returns step-by-step instructions to follow " +
		"(or runs them in an isolated context). Use a skill instead of improvising when one fits. " +
		"Available skills:\n")
	for _, s := range t.skills.List() {
		if s.ModelInvocable() && !s.Conditional() {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Listing())
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (t *skillTool) Schema() json.RawMessage {
	enum, _ := json.Marshal(t.skills.ModelNames())
	return json.RawMessage(fmt.Sprintf(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","enum":%s,"description":"The skill to invoke."},
			"arguments":{"type":"string","description":"Optional arguments for the skill."}
		},
		"required":["name"]
	}`, enum))
}

func (t *skillTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	s, ok := t.skills.Get(a.Name)
	if !ok {
		return "", fmt.Errorf("unknown skill %q; available: %s",
			a.Name, strings.Join(t.skills.ModelNames(), ", "))
	}
	if !s.ModelInvocable() {
		return "", fmt.Errorf("skill %q is user-only (disable-model-invocation)", s.Name)
	}

	body, err := s.Render(ctx, a.Arguments, skill.RenderOpts{})
	if err != nil {
		return "", fmt.Errorf("render skill %q: %w", s.Name, err)
	}

	var skillHooks map[string][]hooks.Matcher
	if len(s.Hooks) > 0 {
		skillHooks, _ = hooks.FromAny(s.Hooks)
	}

	// Apply the skill's tool policy for the rest of the turn. A forked skill runs
	// in its own subagent, so its hooks are attached there (runFork), not here.
	if act := ActivationFrom(ctx); act != nil {
		if allowed := approvableSkillTools(s); len(allowed) > 0 {
			act.Approve(allowed)
		}
		if len(s.DisallowedTools) > 0 {
			act.Disallow(s.DisallowedTools)
		}
		if s.Context != "fork" && len(skillHooks) > 0 {
			act.AddHooks(skillHooks)
		}
	}

	if s.Context == "fork" {
		return t.runFork(ctx, s, body, skillHooks)
	}
	return body, nil
}

// runFork executes the rendered skill in an isolated subagent and returns its
// summary, keeping the skill's work out of the main context.
func (t *skillTool) runFork(ctx context.Context, s *skill.Skill, body string,
	skillHooks map[string][]hooks.Matcher) (string, error) {
	toolNames := normalizeTools(s.AllowedTools)
	if len(toolNames) == 0 {
		toolNames = excluding(t.tools.Names(), TodoToolName)
	}
	subReg := t.tools.Subset(excluding(toolNames, SkillToolName))

	const forkSystem = "You are a focused sub-agent. Carry out the following skill instructions " +
		"to completion using your tools, then report concrete results. Do not ask questions."
	sub := New(t.client, subReg, t.temp, t.confirm, forkSystem)
	if r := HooksFrom(ctx); r != nil {
		sub.SetHooks(r)
		sub.subagentType = s.Name
		if len(skillHooks) > 0 {
			sub.skillHooks = skillHooks
		}
	}

	sink := SinkFrom(ctx)
	ev := Events{}
	if sink != nil {
		sink.AgentStart(s.Name, firstLine(body))
		ev.OnToolStart = func(name string, a json.RawMessage) { sink.SubToolStart(s.Name, name, a) }
		ev.OnToolEnd = func(name, result string, err error) { sink.SubToolEnd(s.Name, name, result, err) }
		ev.OnNotice = func(text string) { sink.SubNotice(s.Name, text) }
	}
	// Same as a delegated subagent: nothing displays a forked skill's deltas, so
	// a mid-stream hiccup is worth retrying rather than surfacing.
	answer, err := sub.Run(llm.WithRetryAfterEmit(ctx), body, ev)
	if sink != nil {
		sink.AgentEnd(answer, err)
	}
	if err != nil {
		return "", fmt.Errorf("skill %q failed: %w", s.Name, err)
	}
	if strings.TrimSpace(answer) == "" {
		answer = "(the skill produced no summary)"
	}
	return answer, nil
}

func normalizeTools(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, normalizeTool(n))
	}
	return out
}

func approvableSkillTools(s *skill.Skill) []string {
	if len(s.AllowedTools) == 0 {
		return nil
	}
	if !s.ProjectLocal {
		return s.AllowedTools
	}
	out := make([]string, 0, len(s.AllowedTools))
	for _, name := range s.AllowedTools {
		if normalizeTool(name) == "bash" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
