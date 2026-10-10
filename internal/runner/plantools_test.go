package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/tools"
)

// planning makes an open ticket, hands it to RUN-1 as its planner, and returns that run.
func (f *fixture) planning(title string) (string, *ticketRun) {
	f.t.Helper()
	v, err := f.tickets.Create(f.project, NewTicket{Title: title})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.tickets.StartPlan(f.project, v.ID, "RUN-1"); err != nil {
		f.t.Fatal(err)
	}
	return v.ID, &ticketRun{project: f.project, ticket: v.ID, run: "RUN-1", plan: true}
}

func runTool(t *testing.T, ts []tools.Tool, name, args string) (string, error) {
	t.Helper()
	for _, tool := range ts {
		if tool.Name() == name {
			if tool.NeedsConfirm() {
				t.Errorf("%s asks for a confirmation", name)
			}
			return tool.Run(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestThePlannerToolsWriteSubticketsOfItsTicketOnly(t *testing.T) {
	f := newFixture(t, t.TempDir())
	api := filepath.Join(f.repo, "api")
	if err := os.MkdirAll(api, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, api, "init", "-q", "-b", "main")
	id, tr := f.planning("goal")
	if _, err := f.tickets.Create(f.project, NewTicket{Title: "other"}); err != nil {
		t.Fatal(err)
	}
	ts := f.tr.planTools(tr)

	out, err := runTool(t, ts, "create_subticket", `{"repo":"api","title":"Schema","body":"Add the table."}`)
	if err != nil || out != "Created TCK-3." {
		t.Fatalf("create = %q, %v", out, err)
	}
	_, err = runTool(t, ts, "create_subticket", `{"repo":"web","title":"UI"}`)
	refusal(t, err, `"web" is not a repository of this project`)
	if _, err := runTool(t, ts, "create_subticket", `{"title":"Handler","dependsOn":["TCK-3"]}`); err != nil {
		t.Fatal(err)
	}
	kid, _ := f.tickets.Get(f.project, "TCK-4")
	if kid.Parent != id || kid.By != "run RUN-1" || kid.Status != TicketOpen || kid.Repo != "" ||
		!slices.Equal(kid.DependsOn, []string{"TCK-3"}) {
		t.Fatalf("subticket = %+v", kid)
	}
	out, err = runTool(t, ts, "list_tickets", `{}`)
	if err != nil || !strings.Contains(out, `"id":"TCK-4"`) || !strings.Contains(out, `"title":"other"`) {
		t.Errorf("list = %s, %v", out, err)
	}
	out, err = runTool(t, ts, "get_ticket", `{"id":"TCK-3"}`)
	if err != nil || !strings.Contains(out, "Add the table.") || !strings.Contains(out, `"repo":"api"`) {
		t.Errorf("get = %s, %v", out, err)
	}
	out, err = runTool(t, ts, "set_dependencies", `{"id":"TCK-4","dependsOn":[]}`)
	if err != nil || out != "TCK-4 waits for []." {
		t.Errorf("set_dependencies = %q, %v", out, err)
	}
	if kid, _ := f.tickets.Get(f.project, "TCK-4"); kid.Status != TicketOpen {
		t.Errorf("set_dependencies moved the subticket to %s", kid.Status)
	}
	_, err = runTool(t, ts, "set_dependencies", `{"id":"TCK-2","dependsOn":["TCK-3"]}`)
	refusal(t, err, "RUN-1 only changes the subtickets of the ticket it plans")
	_, err = runTool(t, ts, "delete_subticket", `{"id":"TCK-2"}`)
	refusal(t, err, "RUN-1 only changes the subtickets of the ticket it plans")
	if out, err := runTool(t, ts, "delete_subticket", `{"id":"TCK-4"}`); err != nil || out != "Deleted TCK-4." {
		t.Fatalf("delete = %q, %v", out, err)
	}
	if _, err := f.tickets.Get(f.project, "TCK-4"); !errors.Is(err, ErrNoTicket) {
		t.Errorf("the deleted subticket = %v", err)
	}
	if _, err := runTool(t, ts, "plan_done", `{"summary":"  "}`); err == nil {
		t.Error("an empty summary was recorded")
	}
	if _, err := runTool(t, ts, "plan_done", `{"summary":" One step. "}`); err != nil {
		t.Fatal(err)
	}
	if s := tr.summary.Load(); s == nil || *s != "One step." {
		t.Errorf("summary = %v", s)
	}
}

func TestThePlannerToolsActOnlyWhileTheirRunPlans(t *testing.T) {
	f := newFixture(t, t.TempDir())
	id, tr := f.planning("goal")
	stale := &ticketRun{project: f.project, ticket: id, run: "RUN-0", plan: true}
	for _, c := range [][2]string{{"create_subticket", `{"title":"x"}`}, {"list_tickets", `{}`}} {
		_, err := runTool(t, f.tr.planTools(stale), c[0], c[1])
		refusal(t, err, "RUN-0 does not plan TCK-1")
	}
	if _, err := f.tickets.PlanReview(f.project, id, "RUN-1", "done"); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{
		{"list_tickets", `{}`}, {"get_ticket", `{"id":"TCK-1"}`}, {"plan_done", `{"summary":"x"}`},
		{"create_subticket", `{"title":"x"}`},
	} {
		_, err := runTool(t, f.tr.planTools(tr), c[0], c[1])
		refusal(t, err, "TCK-1 is review, not planning")
	}
	if tr.summary.Load() != nil {
		t.Error("plan_done recorded a summary outside planning")
	}
}
