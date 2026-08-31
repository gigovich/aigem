package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	model := newFakeModel(t)
	model.fail(http.StatusInternalServerError)
	_, reg := newEnvAndTools(t, project(t))

	notices := make(chan llm.RetryNotice, 4)
	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("m"),
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
	// replies are scripted answers, consumed in order.
	replies []string
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

// sseText is one streamed answer that ends the turn.
func sseText(text string) string {
	body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"content": text}, "finish_reason": "stop",
	}}})
	if err != nil {
		panic(err)
	}
	return "data: " + string(body) + "\n\ndata: [DONE]\n\n"
}

// sseToolCall is one streamed answer that calls a tool, so the turn continues
// with the result appended - which is the only way a test reaches the code that
// runs between model rounds.
func sseToolCall(name, args string) string {
	body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call-1", "type": "function",
			"function": map[string]any{"name": name, "arguments": args},
		}}},
		"finish_reason": "tool_calls",
	}}})
	if err != nil {
		panic(err)
	}
	return "data: " + string(body) + "\n\ndata: [DONE]\n\n"
}

// script queues answers, consumed one per request. Once it runs out the model
// falls back to a plain "ok", so a test only scripts the turns it cares about.
func (f *fakeModel) script(bodies ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, bodies...)
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
			// Answered as a failure, not as an empty success: a request this
			// server could not record must not read to the session as a reply.
			t.Errorf("reading the request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req modelRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("the session sent a body that is not a chat request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		status := f.status
		reply := sseText("ok")
		if len(f.replies) > 0 {
			reply, f.replies = f.replies[0], f.replies[1:]
		}
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModel) ref(model string) *llm.Ref { return llm.NewRef(llm.New(f.srv.URL, model)) }

// fail makes every later request answer with status instead of a completion.
func (f *fakeModel) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

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

// The hooks runner has to reach two places: the agent, which fires the events,
// and the session, which tells the runner which conversation they belong to. A
// hook that echoes the session id back proves both at once - the second is
// invisible from the agent's side, and a hook keyed on the id would write into
// the wrong conversation's bucket without it.
func TestSpecHooksReachTheTurn(t *testing.T) {
	script := filepath.Join(t.TempDir(), "hook.sh")
	writeFile(t, script, "#!/bin/sh\n"+
		`id=$(sed -n 's/.*"session_id":"\([^"]*\)".*/\1/p')`+"\n"+
		`printf '{"hookSpecificOutput":{"additionalContext":"HOOK SAW SID[%s]"}}' "$id"`+"\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	globalSettings(t, `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"`+
		script+`"}]}]}}`)

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
	sent := all.String()
	if !strings.Contains(sent, "HOOK SAW SID[") {
		t.Fatalf("the hook never ran: nothing it wrote reached the model: %+v", reqs[0].Messages)
	}
	if strings.Contains(sent, "HOOK SAW SID[]") {
		t.Fatal("the hook ran without being told which conversation it was for: the session " +
			"never handed the runner its id")
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

// A context window the conversation does not fit in is cut back before the
// request goes out. This is the backstop, gated on the window alone: it proves
// the window reached the agent, and deliberately not that any of the
// compaction policy did - see the test below for that.
func TestSpecContextWindowReachesTheAgent(t *testing.T) {
	model := newFakeModel(t)
	_, reg := newEnvAndTools(t, project(t))

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("m"),
		System:  "SYSTEM",
		Compact: agent.CompactConfig{CtxSize: 100},
	})
	t.Cleanup(s.Local.Close)

	d := drive(t, s)
	// Long enough that the conversation cannot fit the window it was given.
	if err := s.Local.Submit(strings.Repeat("filler text ", 200), nil); err != nil {
		t.Fatal(err)
	}
	waitForNotice(t, d, "to fit the context window",
		"the turn ended without the history being cut to fit: the session was built "+
			"with a context window this conversation does not fit in")
}

// Auto-compaction is the policy: whether it runs at all, and at what share of
// the window. It is a different code path from the backstop above, reached only
// when Auto is on and the percentages are set, and it is what keeps a long
// conversation alive rather than merely legal.
func TestSpecAutoCompactionReachesTheAgent(t *testing.T) {
	model := newFakeModel(t)
	_, reg := newEnvAndTools(t, project(t))

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("m"),
		System:  "SYSTEM",
		// Sized so the last turn opens above CompactAtPct but below the 85% the
		// backstop fires at, so only the policy can produce a notice. KeepTurns
		// is 1 because summarization refuses to touch a conversation shorter
		// than the verbatim tail it is told to keep.
		Compact: agent.CompactConfig{
			Auto: true, CtxSize: 4000, CompactAtPct: 10, EvictAtPct: 5, KeepTurns: 1, KeepTools: 1,
		},
	})
	t.Cleanup(s.Local.Close)

	d := drive(t, s)
	d.turn(strings.Repeat("filler text ", 200))
	d.turn(strings.Repeat("more filler ", 200))
	if err := s.Local.Submit("and now the next turn", nil); err != nil {
		t.Fatal(err)
	}
	waitForNotice(t, d, "compact",
		"the turn ended with no compaction: the session was built with auto-compaction "+
			"and a conversation already past the threshold")
}

// waitForNotice consumes events until a notice matches, failing on the turn
// ending first rather than waiting for a deadline that would say less.
func waitForNotice(t *testing.T, d *driver, substr, whenTurnEnds string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-d.ch:
			if !ok {
				t.Fatal("the session closed mid-turn")
			}
			if ev.Kind == uisession.KindNotice && strings.Contains(ev.Text, substr) {
				return
			}
			if ev.Kind == uisession.KindTurnEnd {
				t.Fatal(whenTurnEnds)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a notice containing %q", substr)
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

// A registry belongs to one conversation. Building a second session on it
// would rebind the delegation, skill and todo tools to the second agent, so the
// first session's registry would drive the second session's turns and route its
// approvals to the wrong clients - with nothing to see.
func TestNewSessionRefusesARegistryThatAlreadyHasASession(t *testing.T) {
	_, reg := newEnvAndTools(t, project(t))

	first := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend()})
	t.Cleanup(first.Local.Close)

	defer func() {
		if recover() == nil {
			t.Fatal("a second session was built on the first session's registry")
		}
	}()
	runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend()})
}

// A zero window in the compaction settings does not mean "use the default" - it
// switches auto-compaction off entirely. A session that names one window must
// not end up with the gauge reading it while nothing ever compacts.
func TestCompactionInheritsTheSessionsContextWindow(t *testing.T) {
	model := newFakeModel(t)
	_, reg := newEnvAndTools(t, project(t))

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("m"),
		System:  "SYSTEM",
		CtxSize: 4000,
		// Deliberately no CtxSize: the session's own is the only one there is.
		Compact: agent.CompactConfig{Auto: true, CompactAtPct: 10, EvictAtPct: 5, KeepTurns: 1},
	})
	t.Cleanup(s.Local.Close)

	d := drive(t, s)
	d.turn(strings.Repeat("filler text ", 200))
	d.turn(strings.Repeat("more filler ", 200))
	if err := s.Local.Submit("and now the next turn", nil); err != nil {
		t.Fatal(err)
	}
	waitForNotice(t, d, "compact",
		"nothing compacted: the compaction settings named no window, and the session's "+
			"was not filled in")
}

// The retry wrapper wraps the handle every caller shares, not the backend
// inside it, so a conversation that switches model keeps riding out provider
// failures. Wrapping the backend would leave the new one bare.
func TestRetriesSurviveAModelSwitch(t *testing.T) {
	model := newFakeModel(t)
	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)
	models, warns := llm.NewRegistry(cwd, llm.LocalProvider(model.srv.URL, "switched-model", 8192, 0))
	if len(warns) != 0 {
		t.Fatalf("model registry warnings: %v", warns)
	}

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("first-model"),
		Models:  models,
		System:  "SYSTEM",
	})
	t.Cleanup(s.Local.Close)
	d := drive(t, s)

	if _, err := s.Local.SwitchModel("local/switched-model", false); err != nil {
		t.Fatalf("switching model: %v", err)
	}
	waitFor(t, d.ch, uisession.KindSessionMeta)

	model.fail(http.StatusInternalServerError)
	if err := s.Local.Submit("hi", nil); err != nil {
		t.Fatal(err)
	}
	waitForWithin(t, d.ch, uisession.KindTurnEnd, 40*time.Second)

	if n := len(model.requests()); n != 3 {
		t.Fatalf("the model after the switch was called %d times, want 3 - the retries "+
			"did not follow the switch", n)
	}
}

// Switching model mid-conversation needs almost everything the session was
// built with: the registry to resolve the reference, the shared backend handle
// to swap the provider inside, the token cap to open it with, and the
// compaction policy to carry onto the new model's window.
func TestSwitchModelUsesWhatTheSessionWasBuiltWith(t *testing.T) {
	model := newFakeModel(t)
	cwd := project(t)
	_, reg := newEnvAndTools(t, cwd)
	// The provider declares a 4000-token window and no cap of its own, so the
	// window below comes from it and the cap can only come from the session.
	models, warns := llm.NewRegistry(cwd, llm.LocalProvider(model.srv.URL, "switched-model", 4000, 0))
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
		// A window nothing can fill, so anything compacted after the switch was
		// compacted against the new model's, by the policy set here.
		Compact: agent.CompactConfig{
			Auto: true, CtxSize: 1 << 20, CompactAtPct: 10, EvictAtPct: 5, KeepTurns: 1,
		},
	})
	t.Cleanup(s.Local.Close)
	d := drive(t, s)

	d.turn(strings.Repeat("filler text ", 200))
	d.turn(strings.Repeat("more filler ", 200))

	info, err := s.Local.SwitchModel("local/switched-model", false)
	if err != nil {
		t.Fatalf("switching model: %v", err)
	}
	if info.ID != "switched-model" {
		t.Fatalf("switched to %q, want switched-model", info.ID)
	}
	if ev := waitFor(t, d.ch, uisession.KindSessionMeta); ev.Ctx != 4000 {
		t.Fatalf("context window after the switch = %d, want the new model's 4000", ev.Ctx)
	}

	// The same conversation that fitted comfortably before the switch is now
	// past the threshold, and only the policy the session was built with can
	// notice.
	if err := s.Local.Submit("and now the next turn", nil); err != nil {
		t.Fatal(err)
	}
	waitForNotice(t, d, "compact",
		"nothing compacted after the switch: the compaction policy did not follow the "+
			"session onto the new model's window")
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

// A subagent runs in its own context and cannot see the conversation, so the
// project's conventions have to be handed to it explicitly. A session that
// forgets them produces an agent that quietly ignores the repository's rules -
// the symptom nobody diagnoses, because everything still works.
func TestSpecProjectTextReachesASubagent(t *testing.T) {
	model := newFakeModel(t)
	env, reg := newEnvAndTools(t, project(t))
	// Turn one delegates; the subagent then answers on its own.
	model.script(sseToolCall(agent.TaskToolName,
		`{"agent_type":"scout","prompt":"look around"}`))

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("m"),
		Agents:  env.Agents,
		Project: "PROJECT CONVENTIONS MARKER",
		System:  "SYSTEM",
		Temp:    0.71,
	})
	t.Cleanup(s.Local.Close)
	drive(t, s).turn("delegate this")

	reqs := model.requests()
	if len(reqs) < 2 {
		t.Fatalf("expected the subagent to make its own request, got %d in total", len(reqs))
	}
	var seen bool
	for i := 1; i < len(reqs); i++ {
		for _, m := range reqs[i].Messages {
			if strings.Contains(m.Content, "PROJECT CONVENTIONS MARKER") {
				seen = true
			}
		}
	}
	if !seen {
		t.Fatalf("the subagent was never told the project's conventions: %+v", reqs[1].Messages)
	}
	// A subagent runs at the session's temperature, not at whatever zero value
	// the delegation tool was built with.
	if reqs[1].Temperature != 0.71 {
		t.Fatalf("the subagent ran at temperature %v, want the session's 0.71", reqs[1].Temperature)
	}
}

// Eviction is the cheap half of compaction: it drops the tool output the model
// has already worked through and keeps the most recent verbatim, so a long
// session stays inside the window without losing what it is holding. Both
// halves of that are policy the session carries, and a session that carries
// neither either sends everything or throws away what it still needs.
func TestSpecEvictionSettingsReachTheAgent(t *testing.T) {
	model := newFakeModel(t)
	cwd := project(t)
	// Three files big enough to dominate the conversation, and distinct enough
	// to tell which survived.
	for _, name := range []string{"one", "two", "three"} {
		writeFile(t, filepath.Join(cwd, name+".txt"),
			strings.Repeat("MARKER-"+name+" filler\n", 300))
	}
	_, reg := newEnvAndTools(t, cwd)
	model.script(
		sseToolCall("read_file", `{"path":"one.txt"}`), sseText("read one"),
		sseToolCall("read_file", `{"path":"two.txt"}`), sseText("read two"),
		sseToolCall("read_file", `{"path":"three.txt"}`), sseText("read three"),
	)

	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("m"),
		System:  "SYSTEM",
		// Summarization is out of reach at 90%, so only eviction can act.
		Compact: agent.CompactConfig{
			Auto: true, CtxSize: 20000, CompactAtPct: 90, EvictAtPct: 1, KeepTurns: 1, KeepTools: 1,
		},
	})
	t.Cleanup(s.Local.Close)

	d := drive(t, s)
	d.turn("read one")
	d.turn("read two")
	d.turn("read three")
	d.turn("and now the next turn")

	reqs := model.requests()
	var sent strings.Builder
	for _, m := range reqs[len(reqs)-1].Messages {
		sent.WriteString(m.Content)
	}
	if strings.Contains(sent.String(), "MARKER-one") {
		t.Error("the oldest tool output was still sent: nothing was evicted")
	}
	if !strings.Contains(sent.String(), "MARKER-three") {
		t.Error("the most recent tool output was evicted too: the keep-recent window " +
			"did not reach the agent, and the model lost what it was working from")
	}
}

// Too few tries turns a provider hiccup into a failed turn; too many turn a
// dead provider into minutes of silence. The count is the whole policy.
func TestNewSessionRetriesExactlyTheConfiguredNumberOfTimes(t *testing.T) {
	model := newFakeModel(t)
	model.fail(http.StatusInternalServerError)
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

	// Compared to the literal, not to the constant that drives the code: a test
	// that measures a value against its own source agrees with every value that
	// source could take.
	if n := len(model.requests()); n != 3 {
		t.Fatalf("the provider was called %d times, want 3", n)
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
	// The bound a test injects its own value for, so nothing else sees the
	// shipped one.
	if got := runner.SessionStartTimeout(); got != 10*time.Second {
		t.Fatalf("sessionStartTimeout = %s, want 10s", got)
	}
}

// A session built for a window a model actually has must use it, not the
// fallback: the fallback is only for a model that declares none.
func TestSpecContextWindowIsUsedWhenGiven(t *testing.T) {
	_, reg := newEnvAndTools(t, project(t))

	s := runner.NewSession(runner.Spec{Tools: reg, Backend: deadBackend(), CtxSize: 262144})
	t.Cleanup(s.Local.Close)

	d := drive(t, s)
	if err := s.Local.Submit("hi", nil); err != nil {
		t.Fatal(err)
	}
	if ev := waitFor(t, d.ch, uisession.KindSessionMeta); ev.Ctx != 262144 {
		t.Fatalf("context window = %d, want the 262144 the session was built with", ev.Ctx)
	}
}
