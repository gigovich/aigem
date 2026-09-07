package runner_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
	"github.com/gigovich/aigem/internal/uisession"
)

// untrustedSkill is a project whose only skill is withheld for lack of
// approval, which is the state every test here starts from.
const untrustedSkill = "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n"

// sessionWithPrompt builds a session whose system prompt is reassembled from
// env, so a test can see what the model is told about the skills - the listing
// and the tool have to agree, and only the prompt says which skills exist.
func sessionWithPrompt(t *testing.T, env *runner.Env, model *fakeModel) (*runner.Session, *tools.Registry) {
	t.Helper()
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	s := runner.NewSession(runner.Spec{
		Tools:   reg,
		Backend: model.ref("m"),
		Skills:  env.Skills,
		Hooks:   env.Hooks,
		RebuildSystem: func() string {
			p, _ := env.SystemPrompt()
			return p
		},
	})
	t.Cleanup(s.Local.Close)
	if err := env.Attach(s); err != nil {
		t.Fatal(err)
	}
	return s, reg
}

func skillToolDescription(t *testing.T, reg *tools.Registry) string {
	t.Helper()
	tool, ok := reg.Get(agent.SkillToolName)
	if !ok {
		t.Fatalf("no skill tool registered, the registry holds %v", reg.Names())
	}
	return tool.Description()
}

// Approving a project's skills has to reach the sessions that are already
// running in it: the catalog, the tool each session offers the model and the
// prompt that lists the skills all describe the same set afterwards.
func TestApproveProjectSkillsUpdatesEveryLiveSession(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", untrustedSkill)
	env, _ := load(t, runner.Options{Cwd: cwd})
	if env.Pending == nil {
		t.Fatal("precondition: the untrusted skill must be withheld")
	}
	firstModel := newFakeModel(t)
	first, firstReg := sessionWithPrompt(t, env, firstModel)
	secondModel := newFakeModel(t)
	second, secondReg := sessionWithPrompt(t, env, secondModel)
	for _, reg := range []*tools.Registry{firstReg, secondReg} {
		if _, ok := reg.Get(agent.SkillToolName); ok {
			t.Fatal("precondition: a withheld skill must not produce a skill tool")
		}
	}

	res, err := env.ApproveProjectSkills()
	if err != nil {
		t.Fatalf("approving the project's skills: %v", err)
	}

	if want := []string{"greet"}; len(res.Loaded) != 1 || res.Loaded[0] != want[0] {
		t.Fatalf("loaded = %v, want %v", res.Loaded, want)
	}
	if env.Pending != nil {
		t.Errorf("skills stayed pending after they were approved: %+v", env.Pending)
	}
	// One discovery per approval, shared by every session: the environment's
	// registry is the one that was replaced, not a new one left beside it.
	if res.Catalog != env.Skills {
		t.Error("the approval left the environment reading a different catalog")
	}
	for _, reg := range []*tools.Registry{firstReg, secondReg} {
		if desc := skillToolDescription(t, reg); !strings.Contains(desc, "say hi nicely") {
			t.Errorf("a session's skill tool describes a different catalog: %q", desc)
		}
	}
	// The prompt is what tells the model the skill exists; a tool it was never
	// told about is a tool it will not call.
	drive(t, first).turn("hi")
	drive(t, second).turn("hi")
	for _, m := range []*fakeModel{firstModel, secondModel} {
		if got := m.system(t, 0); !strings.Contains(got, "say hi nicely") {
			t.Errorf("the system prompt was not rebuilt with the approved skill: %q", got)
		}
	}
}

// Every session reads the one catalog the approval produced rather than a copy
// of its own, so a later change to it is seen by all of them at once.
func TestApprovedSessionsShareOneCatalog(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", untrustedSkill)
	env, _ := load(t, runner.Options{Cwd: cwd})
	_, firstReg := sessionWithPrompt(t, env, newFakeModel(t))
	_, secondReg := sessionWithPrompt(t, env, newFakeModel(t))

	if _, err := env.ApproveProjectSkills(); err != nil {
		t.Fatal(err)
	}
	env.Skills.Remove("greet")

	for _, reg := range []*tools.Registry{firstReg, secondReg} {
		if desc := skillToolDescription(t, reg); strings.Contains(desc, "say hi nicely") {
			t.Errorf("a session kept its own copy of the catalog: %q", desc)
		}
	}
}

// A session that said it no longer wants environment changes does not get them.
func TestApproveProjectSkillsSkipsADetachedSession(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", untrustedSkill)
	env, _ := load(t, runner.Options{Cwd: cwd})
	_, attachedReg := sessionWithPrompt(t, env, newFakeModel(t))
	detached, detachedReg := sessionWithPrompt(t, env, newFakeModel(t))
	env.Detach(detached)

	if _, err := env.ApproveProjectSkills(); err != nil {
		t.Fatal(err)
	}

	if desc := skillToolDescription(t, attachedReg); !strings.Contains(desc, "say hi nicely") {
		t.Errorf("the attached session was not updated: %q", desc)
	}
	if _, ok := detachedReg.Get(agent.SkillToolName); ok {
		t.Error("a detached session was updated anyway")
	}
	// It is still a session: it can be brought to the catalog directly.
	if err := detached.SetSkills(env.Skills); err != nil {
		t.Fatal(err)
	}
	if desc := skillToolDescription(t, detachedReg); !strings.Contains(desc, "say hi nicely") {
		t.Errorf("SetSkills did not update the session: %q", desc)
	}
}

// The tool definitions a turn sends are assembled from the registry on the turn
// goroutine, so a schema swapped in halfway through describes tools the model
// was never shown. The approval is refused whole rather than half-applied.
func TestApprovingSkillsIsRefusedDuringATurn(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", untrustedSkill)
	env, _ := load(t, runner.Options{Cwd: cwd})
	held := newHeldModel(t)
	s, reg := sessionWithPrompt(t, env, held.model)

	if err := s.Local.Submit("hi", nil); err != nil {
		t.Fatal(err)
	}
	held.waitForRequest(t)

	_, err := env.ApproveProjectSkills()
	if !errors.Is(err, uisession.ErrBusy) {
		t.Fatalf("approving mid-turn returned %v, want %v", err, uisession.ErrBusy)
	}
	if _, ok := reg.Get(agent.SkillToolName); ok {
		t.Error("the tool schema was changed under a running turn")
	}
	// Nothing was approved either: the refusal is taken before the decision is
	// persisted, so the person is asked again rather than silently trusting.
	if p, _ := skill.Pending(cwd); p == nil {
		t.Error("a refused approval persisted the trust decision anyway")
	}
	if env.Pending == nil {
		t.Error("a refused approval cleared the pending skills")
	}

	held.release()
	waitFor(t, drive(t, s).ch, uisession.KindTurnEnd)

	if _, err := env.ApproveProjectSkills(); err != nil {
		t.Fatalf("approving once the turn ended: %v", err)
	}
	if desc := skillToolDescription(t, reg); !strings.Contains(desc, "say hi nicely") {
		t.Errorf("the session was not updated after the turn ended: %q", desc)
	}
}

// The session refuses the change itself, not only the environment-level
// approval above it: a front-end holding a session can reach SetSkills with a
// turn in flight, and the registry it would write is the one that turn is
// reading.
func TestSetSkillsIsRefusedDuringATurn(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", untrustedSkill)
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	approved, errs := skill.Discover(cwd)
	if len(errs) != 0 {
		t.Fatalf("discovery after approval: %v", errs)
	}
	empty, _ := skill.Discover(project(t))

	env, _ := load(t, runner.Options{Cwd: cwd})
	held := newHeldModel(t)
	s, reg := sessionWithPrompt(t, env, held.model)
	if _, ok := reg.Get(agent.SkillToolName); !ok {
		t.Fatal("precondition: an approved skill must produce a skill tool")
	}

	if err := s.Local.Submit("hi", nil); err != nil {
		t.Fatal(err)
	}
	held.waitForRequest(t)

	if err := s.SetSkills(empty); !errors.Is(err, uisession.ErrBusy) {
		t.Fatalf("SetSkills mid-turn returned %v, want %v", err, uisession.ErrBusy)
	}
	if _, ok := reg.Get(agent.SkillToolName); !ok {
		t.Error("the running turn lost the tool it was already using")
	}

	held.release()
	waitFor(t, drive(t, s).ch, uisession.KindTurnEnd)

	if err := s.SetSkills(approved); err != nil {
		t.Fatalf("SetSkills once the turn ended: %v", err)
	}
}

// A catalog with nothing left to advertise leaves no tool behind: an enum built
// from a set that is gone offers the model names it cannot invoke.
func TestSetSkillsUnregistersTheToolWhenTheCatalogEmpties(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", untrustedSkill)
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	env, _ := load(t, runner.Options{Cwd: cwd})
	s, reg := sessionWithPrompt(t, env, newFakeModel(t))
	if _, ok := reg.Get(agent.SkillToolName); !ok {
		t.Fatal("precondition: an approved skill must produce a skill tool")
	}

	empty, errs := skill.Discover(project(t))
	if len(errs) != 0 || empty.Len() != 0 {
		t.Fatalf("precondition: an empty project discovered %d skills: %v", empty.Len(), errs)
	}
	if err := s.SetSkills(empty); err != nil {
		t.Fatal(err)
	}

	if _, ok := reg.Get(agent.SkillToolName); ok {
		t.Errorf("the skill tool outlived its catalog, the registry holds %v", reg.Names())
	}
}

// A closed session is gone rather than broken: the approval still stands and is
// not reported as a failure.
func TestApproveProjectSkillsIgnoresAClosedSession(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", untrustedSkill)
	env, _ := load(t, runner.Options{Cwd: cwd})
	_, liveReg := sessionWithPrompt(t, env, newFakeModel(t))
	gone, _ := sessionWithPrompt(t, env, newFakeModel(t))
	gone.Local.Close()

	if _, err := env.ApproveProjectSkills(); err != nil {
		t.Fatalf("a closed session made the approval fail: %v", err)
	}
	if desc := skillToolDescription(t, liveReg); !strings.Contains(desc, "say hi nicely") {
		t.Errorf("the live session was not updated: %q", desc)
	}
}

// heldModel is a provider that does not answer until the test lets it, so a
// turn can be observed while it is still running.
type heldModel struct {
	model   *fakeModel
	srv     *httptest.Server
	got     chan struct{}
	once    sync.Once
	release func()
}

func newHeldModel(t *testing.T) *heldModel {
	t.Helper()
	h := &heldModel{got: make(chan struct{})}
	let := make(chan struct{})
	h.release = sync.OnceFunc(func() { close(let) })
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.once.Do(func() { close(h.got) })
		select {
		case <-let:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte(sseText("ok"))); err != nil {
			t.Errorf("writing the held reply: %v", err)
		}
	}))
	t.Cleanup(h.srv.Close)
	// Registered after Close so it runs before it: httptest.Server.Close waits
	// for the handler, and the handler waits for this. A test that fails before
	// releasing would otherwise hang rather than report.
	t.Cleanup(h.release)
	h.model = &fakeModel{srv: h.srv}
	return h
}

// waitForRequest returns once the turn has reached the provider, which is the
// point at which it is unambiguously running.
func (h *heldModel) waitForRequest(t *testing.T) {
	t.Helper()
	select {
	case <-h.got:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never reached the provider")
	}
}

// agentSystem is the system prompt the model is actually being sent.
func agentSystem(t *testing.T, s *runner.Session) string {
	t.Helper()
	msgs := s.Local.Agent().Messages()
	if len(msgs) == 0 {
		t.Fatal("the agent holds no messages, so there is no system prompt to read")
	}
	return msgs[0].Content
}

// A SessionStart hook's context belongs to the one session it ran for. The
// prompt assembler does not have it - it is the project's, and shared by every
// conversation - so anything that reassembles the prompt mid-session has to put
// it back. A rebuild that does not takes the hook's context away from a
// conversation that has already been told it, and nothing says so: the model
// simply stops knowing something it knew a moment ago.
func TestAPromptRebuiltMidSessionKeepsTheSessionsOwnHookContext(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", untrustedSkill)
	sessionStartHook(t, `{"hookSpecificOutput":{"additionalContext":"PER-SESSION HOOK CONTEXT"}}`)
	env, _ := load(t, runner.Options{Cwd: cwd})
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	// A fixed assembler, so the hook's context can only be in the prompt because
	// the session put it there.
	s := runner.NewSession(runner.Spec{
		Tools: reg, Backend: newFakeModel(t).ref("m"), Skills: env.Skills, Hooks: env.Hooks,
		System: "PROJECT PROMPT", RebuildSystem: func() string { return "PROJECT PROMPT" },
	})
	t.Cleanup(s.Local.Close)
	const marker = "PER-SESSION HOOK CONTEXT"
	if got := agentSystem(t, s); !strings.Contains(got, marker) {
		t.Fatalf("precondition: the session started without its hook's context: %q", got)
	}

	// Attaching reconciles the catalog, and reconciling rebuilds the prompt.
	if err := env.Attach(s); err != nil {
		t.Fatal(err)
	}
	if got := agentSystem(t, s); !strings.Contains(got, marker) {
		t.Fatalf("attaching dropped the hook's context: %q", got)
	}

	if _, err := env.ApproveProjectSkills(); err != nil {
		t.Fatal(err)
	}
	if got := agentSystem(t, s); !strings.Contains(got, marker) {
		t.Fatalf("approving skills dropped the hook's context: %q", got)
	}
	if got := agentSystem(t, s); !strings.HasPrefix(got, "PROJECT PROMPT") {
		t.Fatalf("the assembler's own half is gone: %q", got)
	}
}
