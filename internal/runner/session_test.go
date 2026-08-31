package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

// deadBackend points at a closed port, so a turn that reaches the model fails
// fast instead of hitting a real server.
func deadBackend() *llm.Ref { return llm.NewRef(llm.New("http://127.0.0.1:9", "t")) }

func newEnvAndTools(t *testing.T, cwd string) (*runner.Env, *tools.Registry) {
	t.Helper()
	env, _ := load(t, runner.Options{Cwd: cwd})
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	return env, reg
}

func TestNewSessionRegistersTheAgentsOwnTools(t *testing.T) {
	cwd := project(t)
	env, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: deadBackend(),
		Agents:  env.Agents,
		Skills:  env.Skills,
		Hooks:   env.Hooks,
		System:  systemPrompt(t, env, reg),
	})
	t.Cleanup(s.Local.Close)

	if s.Local.Agent() == nil {
		t.Fatal("the session has no agent")
	}
	if s.Tools != reg {
		t.Fatal("the session reports a registry it was not built with")
	}
	for _, name := range []string{agent.TaskToolName, agent.TodoToolName} {
		if _, ok := reg.Get(name); !ok {
			t.Fatalf("expected %s in the registry, got %v", name, reg.Names())
		}
	}
}

// The delegation tool is what makes subagents possible, so a session built
// without a subagent registry must not advertise one.
func TestNewSessionWithoutSubagentsSkipsTheTaskTool(t *testing.T) {
	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend()})
	t.Cleanup(s.Local.Close)

	if _, ok := reg.Get(agent.TaskToolName); ok {
		t.Fatal("a session with no subagents registered the delegation tool anyway")
	}
}

// A session built where skills already exist advertises them from the first
// turn: nothing re-registers the tool on the ordinary path.
func TestNewSessionRegistersTheSkillToolAtConstruction(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	env, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: deadBackend(),
		Skills:  env.Skills,
		Hooks:   env.Hooks,
	})
	t.Cleanup(s.Local.Close)

	tool, ok := reg.Get(agent.SkillToolName)
	if !ok {
		t.Fatalf("expected the skill tool for a project that has skills, got %v", reg.Names())
	}
	if desc := tool.Description(); !strings.Contains(desc, "say hi nicely") {
		t.Fatalf("the registered tool describes a different catalog: %q", desc)
	}
}

// A project whose only skills are untrusted has none to advertise at launch.
// Approving them mid-session re-runs discovery, and the tool has to be
// registered against the result - that is the whole reason the hook exists.
func TestRegisterSkillToolPicksUpSkillsApprovedMidSession(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n")
	env, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: deadBackend(),
		Skills:  env.Skills,
		Hooks:   env.Hooks,
	})
	t.Cleanup(s.Local.Close)

	if _, ok := reg.Get(agent.SkillToolName); ok {
		t.Fatal("withheld skills must not produce a skill tool")
	}
	if s.RegisterSkillTool == nil {
		t.Fatal("no way to register the skill tool once the skills are approved")
	}

	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	approved, errs := skill.Discover(cwd)
	if len(errs) != 0 {
		t.Fatalf("discovery after approval: %v", errs)
	}
	s.RegisterSkillTool(approved)

	tool, ok := reg.Get(agent.SkillToolName)
	if !ok {
		t.Fatalf("expected the skill tool after approval, got %v", reg.Names())
	}
	if desc := tool.Description(); !strings.Contains(desc, "say hi nicely") {
		t.Fatalf("the registered tool describes a different catalog: %q", desc)
	}
}

// A model that declares no context window still needs one to reason about.
func TestNewSessionDefaultsTheContextWindow(t *testing.T) {
	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend(), CtxSize: 0})
	t.Cleanup(s.Local.Close)

	ch, stop, err := s.Local.Subscribe(uisession.Client{ID: "t", Kind: "test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := s.Local.Submit("hi", nil); err != nil {
		t.Fatal(err)
	}
	ev := waitFor(t, ch, uisession.KindSessionMeta)
	if ev.Ctx != runner.DefaultCtxSize {
		t.Fatalf("expected the default context window %d, got %d", runner.DefaultCtxSize, ev.Ctx)
	}
}

// OnRetry is what turns a provider backoff into something the person waiting
// can see. Nothing else in the session reports it, so an unwired callback is a
// silent multi-second hang.
func TestNewSessionReportsProviderRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)

	notices := make(chan llm.RetryNotice, 4)
	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: llm.NewRef(llm.New(srv.URL, "t")),
		OnRetry: func(n llm.RetryNotice) {
			select {
			case notices <- n:
			default:
			}
		},
	})
	t.Cleanup(s.Local.Close)

	if err := s.Local.Submit("hi", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-notices:
		if n.Attempt < 1 || n.Attempts < 2 {
			t.Fatalf("a retry notice that describes no retry: %+v", n)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a failing provider produced no retry notice")
	}
	// The turn is mid-backoff; let it go rather than paying for the rest.
	s.Local.Interrupt()
}

// waitFor takes events until one matches, so a test can assert on a kind
// without listing every event that legitimately precedes it, and fails rather
// than hanging when none arrives.
func waitFor(t *testing.T, ch <-chan uisession.Event, kind uisession.Kind) uisession.Event {
	t.Helper()
	return waitForWithin(t, ch, kind, 10*time.Second)
}

func waitForWithin(t *testing.T, ch <-chan uisession.Event, kind uisession.Kind,
	within time.Duration) uisession.Event {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("the event channel closed before a %s event", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s event", kind)
		}
	}
}

// systemPrompt builds the prompt and marks what it injected, which is the pair
// of steps a front-end always does together.
func systemPrompt(t *testing.T, env *runner.Env, reg *tools.Registry) string {
	t.Helper()
	p, injected := env.SystemPrompt()
	reg.MarkInContext(injected)
	return p
}

// fakeModel is an OpenAI-compatible endpoint that records what the session
// actually sent. It is the only way most of Spec is observable: a field that
// never reaches the provider is a field the session silently dropped.
type fakeModel struct {
	srv *httptest.Server

	mu   sync.Mutex
	reqs []modelRequest
	// status, when non-zero, is answered instead of a completion.
	status int
}

type modelRequest struct {
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	Messages    []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func newFakeModel(t *testing.T) *fakeModel {
	t.Helper()
	f := &fakeModel{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the completions endpoint is answered. Everything else - the
		// tokenizer a local server would offer - is absent, so token counts fall
		// back to the deterministic estimate rather than to whatever this
		// handler would have replied.
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the request body: %v", err)
			return
		}
		var req modelRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("the session sent a body that is not a chat request: %v", err)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		status := f.status
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModel) ref(model string) *llm.Ref { return llm.NewRef(llm.New(f.srv.URL, model)) }

func (f *fakeModel) requests() []modelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]modelRequest(nil), f.reqs...)
}

func (f *fakeModel) system(t *testing.T, n int) string {
	t.Helper()
	reqs := f.requests()
	if len(reqs) <= n {
		t.Fatalf("the session made %d requests, wanted at least %d", len(reqs), n+1)
	}
	for _, m := range reqs[n].Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	t.Fatalf("request %d carried no system message: %+v", n, reqs[n].Messages)
	return ""
}

// driver runs turns against a session and waits for each to finish, so an
// assertion about what reached the provider is made after the request was sent.
//
// The subscription is opened once and before the first turn: opening one per
// turn would replay the history from zero, and the second turn would be handed
// the first one's turn_end before it had sent anything.
type driver struct {
	t  *testing.T
	s  *runner.Session
	ch <-chan uisession.Event
}

func drive(t *testing.T, s *runner.Session) *driver {
	t.Helper()
	ch, stop, err := s.Local.Subscribe(uisession.Client{ID: "test", Kind: "test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return &driver{t: t, s: s, ch: ch}
}

func (d *driver) turn(text string) {
	d.t.Helper()
	if err := d.s.Local.Submit(text, nil); err != nil {
		d.t.Fatal(err)
	}
	waitFor(d.t, d.ch, uisession.KindTurnEnd)
}

// The system prompt, the temperature and the model are the three things a
// session is built with that the provider is entitled to see. None of them is
// observable anywhere else.
func TestSpecReachesTheProvider(t *testing.T) {
	model := newFakeModel(t)
	_, reg := newEnvAndTools(t, project(t))

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("some-model"),
		System:  "SYSTEM MARKER",
		Temp:    0.71,
	})
	t.Cleanup(s.Local.Close)
	drive(t, s).turn("hi")

	reqs := model.requests()
	if len(reqs) != 1 {
		t.Fatalf("expected one request, got %d", len(reqs))
	}
	if got := model.system(t, 0); got != "SYSTEM MARKER" {
		t.Fatalf("system prompt sent = %q, want SYSTEM MARKER", got)
	}
	if reqs[0].Temperature != 0.71 {
		t.Fatalf("temperature sent = %v, want 0.71", reqs[0].Temperature)
	}
	if reqs[0].Model != "some-model" {
		t.Fatalf("model sent = %q, want some-model", reqs[0].Model)
	}
}

// RebuildSystem is what makes an edit to AGENTS.md take effect without a
// restart. A session built without it is pinned to its launch-time prompt, and
// nothing about the session says so.
func TestRebuildSystemReplacesThePrompt(t *testing.T) {
	model := newFakeModel(t)
	_, reg := newEnvAndTools(t, project(t))

	s := runner.NewSession(runner.Spec{
		Tools:         reg,
		Backend:       model.ref("m"),
		System:        "FIRST PROMPT",
		RebuildSystem: func() string { return "SECOND PROMPT" },
	})
	t.Cleanup(s.Local.Close)

	d := drive(t, s)
	d.turn("one")
	if got := model.system(t, 0); got != "FIRST PROMPT" {
		t.Fatalf("first turn used %q, want FIRST PROMPT", got)
	}
	s.Local.RebuildSystem()
	d.turn("two")
	if got := model.system(t, 1); got != "SECOND PROMPT" {
		t.Fatalf("after a rebuild the session sent %q, want SECOND PROMPT", got)
	}
}

// The hooks runner reaches the agent, not only the session: a UserPromptSubmit
// hook that adds context has to be in what the model is asked.
func TestSpecHooksReachTheTurn(t *testing.T) {
	cfg := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"printf",` +
		`"args":["%s","{\"hookSpecificOutput\":{\"additionalContext\":\"HOOK SAW IT\"}}"]}]}]}}`
	globalSettings(t, cfg)

	model := newFakeModel(t)
	env, reg := newEnvAndTools(t, project(t))
	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("m"),
		Hooks:   env.Hooks,
		System:  "SYSTEM",
	})
	t.Cleanup(s.Local.Close)
	drive(t, s).turn("hi")

	reqs := model.requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the provider")
	}
	var all strings.Builder
	for _, m := range reqs[0].Messages {
		all.WriteString(m.Content)
	}
	if !strings.Contains(all.String(), "HOOK SAW IT") {
		t.Fatalf("the hook's context never reached the model: %+v", reqs[0].Messages)
	}
}

// The conversation carries the name the SessionStart hook gave it.
func TestSpecTitleNamesTheConversation(t *testing.T) {
	model := newFakeModel(t)
	_, reg := newEnvAndTools(t, project(t))

	s := runner.NewSession(runner.Spec{Tools: reg, Backend: model.ref("m"), Title: "NAMED"})
	t.Cleanup(s.Local.Close)
	drive(t, s).turn("hi")

	meta := s.Local.Meta()
	if meta.Title != "NAMED" {
		t.Fatalf("conversation title = %q, want NAMED", meta.Title)
	}
	// The model reference is what a resumed conversation is reopened on, so a
	// session that records none comes back on whatever the default is.
	if !strings.Contains(meta.Model, "m") {
		t.Fatalf("conversation model = %q, want the model the session was built on", meta.Model)
	}
}

// A path-gated skill is loaded only when the turn touches a matching file, and
// the agent is the thing that watches for it. A session that never arms the
// watch has skills that simply never activate, with nothing to see.
func TestSpecArmsThePathGatedSkills(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "onpy",
		"---\nname: onpy\ndescription: python conventions\npaths: \"**/*.py\"\n---\nUse tabs.\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	env, reg := newEnvAndTools(t, cwd)
	if len(env.Skills.Conditional()) != 1 {
		t.Fatalf("the fixture is not a path-gated skill: %+v", env.Skills.Conditional())
	}

	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend(), Skills: env.Skills})
	t.Cleanup(s.Local.Close)

	if got := s.Local.Agent().WatchedSkills(); len(got) != 1 || got[0].Name != "onpy" {
		t.Fatalf("watched skills = %+v, want the project's path-gated skill", got)
	}
}

// The compaction settings are what keep a conversation inside the context
// window. A session built without them sends the whole history until the
// provider refuses it, and the only sign is a failure much later.
func TestSpecCompactionReachesTheAgent(t *testing.T) {
	model := newFakeModel(t)
	_, reg := newEnvAndTools(t, project(t))

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("m"),
		System:  "SYSTEM",
		// A context window one turn cannot fit in, so the first turn is already
		// over the threshold.
		Compact: agent.CompactConfig{Auto: true, CtxSize: 100, CompactAtPct: 1, EvictAtPct: 1},
	})
	t.Cleanup(s.Local.Close)

	d := drive(t, s)
	// Long enough that the conversation cannot fit the window it was given.
	if err := s.Local.Submit(strings.Repeat("filler text ", 200), nil); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-d.ch:
			if !ok {
				t.Fatal("the session closed before the history was cut to fit")
			}
			if ev.Kind == uisession.KindNotice && strings.Contains(ev.Text, "to fit the context window") {
				return
			}
			if ev.Kind == uisession.KindTurnEnd {
				t.Fatal("the turn ended without the history being cut to fit: the session was " +
					"built with a context window this conversation does not fit in")
			}
		case <-deadline:
			t.Fatal("timed out waiting for a compaction notice")
		}
	}
}

// The session takes over the registry it is built with: it is what records the
// files a turn changed, so the "review what this session did" list exists at
// all. A session handed no registry silently records nothing.
func TestNewSessionTakesOverTheRegistry(t *testing.T) {
	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)

	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend()})
	t.Cleanup(s.Local.Close)

	write, ok := reg.Get("write_file")
	if !ok {
		t.Fatalf("no write_file tool: %v", reg.Names())
	}
	args, err := json.Marshal(map[string]any{"path": "notes.txt", "content": "written by a tool\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write.Run(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	arts := s.Local.Artifacts()
	if len(arts) != 1 {
		t.Fatalf("the session recorded %d changed files, want 1: %+v", len(arts), arts)
	}
}

// Switching model mid-conversation is the one operation that needs almost
// everything the session was built with at once: the registry to resolve the
// reference, the shared backend handle to swap the provider inside, the token
// cap to open it with, and the compaction settings to carry over to the agent
// it rebuilds.
func TestSwitchModelUsesWhatTheSessionWasBuiltWith(t *testing.T) {
	model := newFakeModel(t)
	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)
	models, warns := llm.NewRegistry(cwd, llm.LocalProvider(model.srv.URL, "switched-model", 100, 0))
	if len(warns) != 0 {
		t.Fatalf("model registry warnings: %v", warns)
	}

	s := runner.NewSession(runner.Spec{
		Tools:     reg,
		Backend:   model.ref("first-model"),
		Models:    models,
		MaxTokens: 321,
		CtxSize:   4096,
		System:    "SYSTEM",
		// Roomy before the switch, so anything cut afterwards was cut because
		// the switch carried these settings onto the model's own window.
		Compact: agent.CompactConfig{Auto: true, CtxSize: 1 << 20, CompactAtPct: 1, EvictAtPct: 1},
	})
	t.Cleanup(s.Local.Close)
	d := drive(t, s)

	info, err := s.Local.SwitchModel("local/switched-model", false)
	if err != nil {
		t.Fatalf("switching model: %v", err)
	}
	if info.ID != "switched-model" {
		t.Fatalf("switched to %q, want switched-model", info.ID)
	}

	// Long enough that the window has to be cut to fit, which is only true if
	// the compaction settings survived the agent being rebuilt.
	if err := s.Local.Submit(strings.Repeat("filler text ", 400), nil); err != nil {
		t.Fatal(err)
	}
	fitted := false
	for !fitted {
		ev, ok := <-d.ch
		if !ok {
			t.Fatal("the session closed mid-turn")
		}
		switch {
		case ev.Kind == uisession.KindNotice && strings.Contains(ev.Text, "to fit the context window"):
			fitted = true
		case ev.Kind == uisession.KindTurnEnd:
			t.Fatal("the rebuilt agent lost the compaction settings: nothing was cut to fit")
		}
	}
	waitFor(t, d.ch, uisession.KindTurnEnd)

	reqs := model.requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the provider after the switch")
	}
	last := reqs[len(reqs)-1]
	if last.Model != "switched-model" {
		t.Fatalf("the turn after the switch went to %q, want switched-model - the session did "+
			"not swap the provider inside the handle every caller shares", last.Model)
	}
	// The provider declares no cap, so the only one left is the session's own.
	if last.MaxTokens != 321 {
		t.Fatalf("max_tokens sent = %d, want 321 - the cap the session was built with", last.MaxTokens)
	}
}

// Too few tries turns a provider hiccup into a failed turn; too many turn a
// dead provider into minutes of silence. The count is the whole policy.
func TestNewSessionRetriesExactlyTheConfiguredNumberOfTimes(t *testing.T) {
	model := newFakeModel(t)
	model.status = http.StatusInternalServerError
	_, reg := newEnvAndTools(t, project(t))

	s := runner.NewSession(runner.Spec{Tools: reg, Backend: model.ref("m")})
	t.Cleanup(s.Local.Close)

	ch, stop, err := s.Local.Subscribe(uisession.Client{ID: "test", Kind: "test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := s.Local.Submit("hi", nil); err != nil {
		t.Fatal(err)
	}
	// The backoff is 2s then 4s, so the turn takes about six seconds to give up.
	waitForWithin(t, ch, uisession.KindTurnEnd, 40*time.Second)

	if n := len(model.requests()); n != runner.RetryAttempts {
		t.Fatalf("the provider was called %d times, want %d", n, runner.RetryAttempts)
	}
}

// Both constants are policy, so they are pinned to a literal rather than to
// themselves: a test that compares a value against the constant it came from
// agrees with every value the constant could take.
func TestPolicyConstants(t *testing.T) {
	if runner.DefaultCtxSize != 8192 {
		t.Fatalf("DefaultCtxSize = %d, want 8192", runner.DefaultCtxSize)
	}
	if runner.RetryAttempts != 3 {
		t.Fatalf("RetryAttempts = %d, want 3", runner.RetryAttempts)
	}
}
