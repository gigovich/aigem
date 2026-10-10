package runner

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/uisession"
)

func (f *fixture) openTicket(title string) string {
	f.t.Helper()
	v, err := f.tickets.Create(f.project, NewTicket{Title: title, Body: "Plan " + title + "."})
	if err != nil {
		f.t.Fatal(err)
	}
	return v.ID
}

func (f *fixture) plan(id string) RunView {
	f.t.Helper()
	v, err := f.tr.Plan(context.Background(), f.project, id)
	if err != nil {
		f.t.Fatalf("Plan %s: %v", id, err)
	}
	return v
}

// firstMessage is the first thing a run was told.
func (f *fixture) firstMessage(run string) string {
	f.t.Helper()
	evs, err := f.runs.Events(run, 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, ev := range evs {
		if ev.Kind == uisession.KindUserMessage {
			return ev.Text
		}
	}
	return ""
}

func TestAPlannerRunSplitsTheTicketAndHandsItToAPerson(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	id := f.openTicket("big goal")
	f.script.then(
		call("create_subticket", `{"title":"Schema"}`),
		call("create_subticket", `{"title":"Handler","dependsOn":["TCK-2"]}`),
		call("write_file", `{"path":"x.txt","content":"x"}`),
		call("plan_done", `{"summary":"Two steps."}`),
		say("Planned."),
	)
	v := f.plan(id)
	if v.TicketID != id || v.Mode != ModeAutonomous || v.Root != f.repo || v.Worktree != "" {
		t.Fatalf("run = %+v", v)
	}
	got := f.next(id, 0)
	if got.Status != TicketReview || lastComment(got) != "Two steps." || got.Progress == nil ||
		got.Progress.Total != 2 || !slices.Equal(got.Runs, []string{v.ID}) {
		t.Fatalf("ticket = %s %q %+v", got.Status, lastComment(got), got.Progress)
	}
	if k, _ := f.tickets.Get(f.project, "TCK-3"); k.Status != TicketOpen || k.By != "run "+v.ID ||
		!slices.Equal(k.DependsOn, []string{"TCK-2"}) {
		t.Errorf("draft = %+v", k)
	}
	if pathExists(filepath.Join(f.repo, "x.txt")) {
		t.Error("the planner wrote a file")
	}
	if rv, _ := f.runs.Get(v.ID); !rv.Live {
		t.Error("the planner run ended at review; Revise needs it")
	}
	if msg := f.firstMessage(v.ID); !strings.Contains(msg, "# TCK-1: big goal") || !strings.Contains(msg, planRule) {
		t.Errorf("first message = %q", msg)
	}
	waitUntil(t, func() bool { return f.toldAbout("review: Two steps.") })
}

func TestAPlannerTurnAlwaysEndsInReview(t *testing.T) {
	f := newFixture(t, gitRepo(t, "main"))
	ask := f.openTicket("ask")
	f.script.then(say("Should the API be versioned?"))
	f.plan(ask)
	if got := f.next(ask, 0); got.Status != TicketReview || lastComment(got) != "Should the API be versioned?" {
		t.Fatalf("without plan_done = %s %q", got.Status, lastComment(got))
	}

	broken := f.openTicket("broken")
	f.script.then(call("plan_done", `{"summary":"Done."}`), func(context.Context) (llm.Message, error) {
		return llm.Message{}, errors.New("the model is gone")
	})
	f.plan(broken)
	if got := f.next(broken, 0); got.Status != TicketReview ||
		!strings.HasPrefix(lastComment(got), "the turn ended with an error: ") {
		t.Fatalf("an error after plan_done = %s %q", got.Status, lastComment(got))
	}

	slow := f.openTicket("slow")
	f.script.then(hold())
	v := f.plan(slow)
	waitUntil(t, func() bool { return f.script.left() == 0 })
	if err := f.runs.Apply(v.ID, RunOp{Op: OpInterrupt}); err != nil {
		t.Fatal(err)
	}
	if got := f.next(slow, 0); got.Status != TicketReview || lastComment(got) != "interrupted" {
		t.Fatalf("interrupted = %s %q", got.Status, lastComment(got))
	}

	f.script.then(call("plan_done", `{"summary":"Asked and answered."}`), say("ok"))
	if err := f.runs.Apply(v.ID, RunOp{Op: OpSubmit, Text: "go on"}); err != nil {
		t.Fatal(err)
	}
	if got := f.next(slow, 1); got.Status != TicketReview || lastComment(got) != "Asked and answered." {
		t.Fatalf("a person's turn in the planner = %s %q", got.Status, lastComment(got))
	}
}

func TestPlanRefusesATicketThatCannotBePlanned(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, gitRepo(t, "main"))
	goal := f.openTicket("goal")
	kid, err := f.tickets.Create(f.project, NewTicket{Title: "kid", Parent: goal})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.tr.Plan(ctx, f.project, kid.ID)
	refusal(t, err, "TCK-2 is a subticket; only a top-level ticket is planned")
	_, err = f.tr.Plan(ctx, f.project, goal)
	refusal(t, err, "TCK-1 already has subtickets")
	ready := f.ready("ready")
	_, err = f.tr.Plan(ctx, f.project, ready)
	refusal(t, err, "TCK-3 is ready, not open")

	f.script.then(say("Stuck."))
	run := f.start(ready)
	f.next(ready, 0)
	if _, err := f.tickets.Update(f.project, ready, TicketPatch{Status: ptr(TicketOpen)}); err != nil {
		t.Fatal(err)
	}
	_, err = f.tr.Plan(ctx, f.project, ready)
	refusal(t, err, ready+" has a live run "+run.ID+"; stop it first")

	out, err := f.tickets.Create(f.project, NewTicket{Repo: "../elsewhere", Title: "outside"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.tr.Plan(ctx, f.project, out.ID)
	refusal(t, err, `"../elsewhere" is not a repository of this project`)
}

func TestTwoPlanClicksOpenOnePlanner(t *testing.T) {
	f := newFixture(t, t.TempDir())
	id := f.openTicket("goal")
	f.script.then(hold())
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() { _, errs[i] = f.tr.Plan(context.Background(), f.project, id) })
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("plans = %v, want exactly one to start", errs)
	}
	refused := errs[0]
	if refused == nil {
		refused = errs[1]
	}
	refusal(t, refused, "TCK-1 is planning, not open")
	if got, _ := f.tickets.Get(f.project, id); len(got.Runs) != 1 {
		t.Errorf("runs = %v, want one", got.Runs)
	}
	if n := len(f.runs.List()); n != 1 {
		t.Errorf("%d runs are left, want the planner only", n)
	}
}

func TestThePlannerPromptCarriesTheTicketRepositoriesDraftAndRule(t *testing.T) {
	got := planPrompt(Ticket{ID: "TCK-1", Title: "goal", Body: "Make it fast.",
		Comments: []Comment{{By: "you", Text: "Split it in two."}}},
		[]TicketView{{Ticket: Ticket{ID: "TCK-2", Repo: "api", Title: "Schema"}},
			{Ticket: Ticket{ID: "TCK-3", Title: "Handler", DependsOn: []string{"TCK-2"}}}},
		[]Repository{{Dir: "/p"}, {Name: "api", Dir: "/p/api"}})
	for _, want := range []string{
		"# TCK-1: goal", "Make it fast.", "you: Split it in two.", "## Repositories",
		`- "" (the project directory)`, "- api\n", "## Your draft", "- TCK-2: Schema (repo api)\n",
		"- TCK-3: Handler (waits for TCK-2)\n", planRule,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, got)
		}
	}
	if first := planPrompt(Ticket{ID: "TCK-1", Title: "goal"}, nil, nil); strings.Contains(first, "draft") {
		t.Errorf("a first plan has a draft section:\n%s", first)
	}
}
