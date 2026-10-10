package web

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ticketsBackend is a fake that records what the handlers passed and answers from a slice.
type ticketsBackend struct {
	*projectsBackend
	tmu     sync.Mutex
	tickets []Ticket
	patched TicketPatch
	decided string
}

func (b *ticketsBackend) Tickets(_ context.Context, project string) ([]Ticket, error) {
	if project != "PRJ-1" {
		return nil, ErrNoProject
	}
	b.tmu.Lock()
	defer b.tmu.Unlock()
	return b.tickets, nil
}

func (b *ticketsBackend) Ticket(_ context.Context, project, id string) (Ticket, error) {
	b.tmu.Lock()
	defer b.tmu.Unlock()
	for _, t := range b.tickets {
		if t.ID == id {
			return t, nil
		}
	}
	return Ticket{}, ErrNoTicket
}

func (b *ticketsBackend) CreateTicket(_ context.Context, project string, req NewTicket) (Ticket, error) {
	if req.Parent == "TCK-2" {
		return Ticket{}, Conflict("TCK-2 is a subticket and cannot have subtickets")
	}
	t := Ticket{ID: "TCK-9", Title: req.Title, Status: "open", By: "you", DependsOn: req.DependsOn}
	return t, nil
}

func (b *ticketsBackend) UpdateTicket(_ context.Context, project, id string, req TicketPatch) (Ticket, error) {
	b.tmu.Lock()
	b.patched = req
	b.tmu.Unlock()
	return Ticket{ID: id, Status: "ready"}, nil
}

func (b *ticketsBackend) CommentTicket(_ context.Context, project, id, text string) (Ticket, error) {
	return Ticket{ID: id, Comments: []TicketComment{{By: "you", Text: text}}}, nil
}

func (b *ticketsBackend) DeleteTicket(_ context.Context, project, id string) error {
	if id == "TCK-1" {
		return Conflict("TCK-1 has subtickets; close it instead")
	}
	return nil
}

func newTicketsServer(t *testing.T) (*Server, *ticketsBackend) {
	t.Helper()
	_, pb := newProjectsServer(t)
	b := &ticketsBackend{projectsBackend: pb, tickets: []Ticket{
		{ID: "TCK-1", Title: "goal", Status: "ready", Progress: &TicketProgress{Done: 0, Total: 1}},
		{ID: "TCK-2", Title: "kid", Status: "ready", Parent: "TCK-1", Runnable: true},
		{ID: "TCK-3", Title: "done", Status: "done"},
	}}
	return newTestServer(t, Config{Backend: b}), b
}

func TestTheTicketRoutesAnswerAndFilter(t *testing.T) {
	srv, b := newTicketsServer(t)
	_, meta := getMeta(t, srv)
	if !meta.Features["tickets"] {
		t.Error("the feature map does not name tickets")
	}

	all := decode[[]Ticket](t, api(t, srv, http.MethodGet, "/api/projects/PRJ-1/tickets", ""))
	if len(all) != 3 || !all[1].Runnable || all[0].Progress.Total != 1 {
		t.Fatalf("list = %+v", all)
	}
	ready := decode[[]Ticket](t, api(t, srv, http.MethodGet, "/api/projects/PRJ-1/tickets?status=ready", ""))
	if len(ready) != 2 {
		t.Errorf("?status=ready = %d tickets, want 2", len(ready))
	}
	kids := decode[[]Ticket](t, api(t, srv, http.MethodGet, "/api/projects/PRJ-1/tickets?parent=TCK-1", ""))
	if len(kids) != 1 || kids[0].ID != "TCK-2" {
		t.Errorf("?parent=TCK-1 = %+v", kids)
	}
	if res := api(t, srv, http.MethodGet, "/api/projects/PRJ-9/tickets", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown project = %d, want 404", res.StatusCode)
	}
	res := api(t, srv, http.MethodGet, "/api/projects/PRJ-1/tickets/TCK-7", "")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown ticket = %d, want 404", res.StatusCode)
	}

	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets", `{"title":"new","dependsOn":["TCK-3"]}`)
	if res.StatusCode != http.StatusCreated || decode[Ticket](t, res).ID != "TCK-9" {
		t.Errorf("create = %d", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets", `{"title":"  "}`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(readBody(t, res), "title is required") {
		t.Errorf("empty title = %d, want 400", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets", `{"title":"x","parent":"TCK-2"}`)
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "cannot have subtickets") {
		t.Errorf("a refused rule = %d, want 409 with the sentence", res.StatusCode)
	}
	big := strings.Repeat("a", 70<<10)
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets", `{"title":"x","body":"`+big+`"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a 70 KiB body = %d, want 400", res.StatusCode)
	}

	res = api(t, srv, http.MethodPatch, "/api/projects/PRJ-1/tickets/TCK-2", `{"status":"ready","dependsOn":[]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d", res.StatusCode)
	}
	b.tmu.Lock()
	if b.patched.Status == nil || *b.patched.Status != "ready" || b.patched.DependsOn == nil {
		t.Errorf("patched = %+v, want status and an empty dependsOn passed through", b.patched)
	}
	b.tmu.Unlock()
	res = api(t, srv, http.MethodPatch, "/api/projects/PRJ-1/tickets/TCK-2", `{"title":"renamed"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("patching the title = %d, want 400 (not editable in part 1)", res.StatusCode)
	}

	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-2/comments", `{"text":"hi"}`)
	if res.StatusCode != http.StatusCreated {
		t.Errorf("comment = %d, want 201", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-2/comments",
		`{"text":"`+strings.Repeat("a", 17<<10)+`"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a 17 KiB comment = %d, want 400", res.StatusCode)
	}

	res = api(t, srv, http.MethodDelete, "/api/projects/PRJ-1/tickets/TCK-3", "")
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", res.StatusCode)
	}
	res = api(t, srv, http.MethodDelete, "/api/projects/PRJ-1/tickets/TCK-1", "")
	if res.StatusCode != http.StatusConflict {
		t.Errorf("delete refused = %d, want 409", res.StatusCode)
	}
}

func TestTheTicketRoutesRefuseOtherMethodsAndNeedTheFeature(t *testing.T) {
	srv, _ := newTicketsServer(t)
	for path, method := range map[string]string{
		"/api/projects/PRJ-1/tickets":                http.MethodPut,
		"/api/projects/PRJ-1/tickets/TCK-1":          http.MethodPost,
		"/api/projects/PRJ-1/tickets/TCK-1/comments": http.MethodGet,
	} {
		if res := api(t, srv, method, path, ""); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, path, res.StatusCode)
		}
	}
	projectsOnly, _ := newProjectsServer(t)
	res := api(t, projectsOnly, http.MethodGet, "/api/projects/PRJ-1/tickets", "")
	if res.StatusCode != http.StatusNotImplemented {
		t.Errorf("a backend without the seam = %d, want 501", res.StatusCode)
	}
}

func (b *ticketsBackend) RunTicket(_ context.Context, project, id string) (Run, error) {
	switch id {
	case "TCK-3":
		return Run{}, Conflict("TCK-3 is not runnable")
	case "TCK-7":
		return Run{}, ErrNoTicket
	}
	return Run{ID: "RUN-5", Mode: "autonomous", TicketID: id, Status: "open", Live: true}, nil
}

func (b *ticketsBackend) MergeTicket(_ context.Context, project, id string) (Ticket, error) {
	if id == "TCK-1" {
		return Ticket{}, Conflict("the main checkout has uncommitted changes")
	}
	return Ticket{ID: id, Status: "done"}, nil
}

func (b *ticketsBackend) Worktrees(_ context.Context, project string) ([]Worktree, error) {
	if project != "PRJ-1" {
		return nil, ErrNoProject
	}
	return nil, nil
}

func (b *ticketsBackend) DiscardWorktree(_ context.Context, project, name string) error {
	switch name {
	case "TCK-2":
		return Conflict("TCK-2 is worked on by the live run RUN-5; stop it first")
	case "TCK-9":
		return ErrNoWorktree
	}
	return nil
}

func TestTheTicketRunRoutesAnswerAndRefuse(t *testing.T) {
	srv, _ := newTicketsServer(t)
	res := api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-2/run", "")
	if res.StatusCode != http.StatusCreated || decode[Run](t, res).TicketID != "TCK-2" {
		t.Errorf("run = %d", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-3/run", "")
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "not runnable") {
		t.Errorf("a refused run = %d, want 409 with the sentence", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-7/run", "")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown ticket = %d, want 404", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-2/merge", "")
	if res.StatusCode != http.StatusOK || decode[Ticket](t, res).Status != "done" {
		t.Errorf("merge = %d", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets/TCK-1/merge", "")
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "uncommitted changes") {
		t.Errorf("a refused merge = %d, want 409", res.StatusCode)
	}
	res = api(t, srv, http.MethodGet, "/api/projects/PRJ-1/worktrees", "")
	if body := strings.TrimSpace(readBody(t, res)); res.StatusCode != http.StatusOK || body != "[]" {
		t.Errorf("worktrees = %d %s, want 200 []", res.StatusCode, body)
	}
	if res := api(t, srv, http.MethodGet, "/api/projects/PRJ-9/worktrees", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown project = %d, want 404", res.StatusCode)
	}
	for name, want := range map[string]int{
		"TCK-1": http.StatusNoContent, "TCK-2": http.StatusConflict, "TCK-9": http.StatusNotFound,
	} {
		if res := api(t, srv, http.MethodDelete, "/api/projects/PRJ-1/worktrees/"+name, ""); res.StatusCode != want {
			t.Errorf("discard %s = %d, want %d", name, res.StatusCode, want)
		}
	}
	for path, method := range map[string]string{
		"/api/projects/PRJ-1/tickets/TCK-2/run":   http.MethodGet,
		"/api/projects/PRJ-1/tickets/TCK-2/merge": http.MethodGet,
		"/api/projects/PRJ-1/worktrees":           http.MethodPost,
		"/api/projects/PRJ-1/worktrees/TCK-1":     http.MethodGet,
	} {
		if res := api(t, srv, method, path, ""); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, path, res.StatusCode)
		}
	}
}

func (b *ticketsBackend) PlanTicket(_ context.Context, project, id string) (Run, error) {
	if id == "TCK-2" {
		return Run{}, Conflict("TCK-2 is a subticket; only a top-level ticket is planned")
	}
	return Run{ID: "RUN-6", Mode: "autonomous", TicketID: id, Status: "open", Live: true}, nil
}

func (b *ticketsBackend) ApproveTicket(_ context.Context, project, id string) (Ticket, error) {
	if id == "TCK-3" {
		return Ticket{}, Conflict("TCK-3 is done, not in review")
	}
	return Ticket{ID: id, Status: "ready"}, nil
}

func (b *ticketsBackend) RejectTicket(_ context.Context, project, id, reason string) (Ticket, error) {
	if id == "TCK-3" {
		return Ticket{}, Conflict("TCK-3 is done, not in review")
	}
	b.tmu.Lock()
	b.decided = reason
	b.tmu.Unlock()
	return Ticket{ID: id, Status: "open"}, nil
}

func (b *ticketsBackend) ReviseTicket(_ context.Context, project, id, text string) (Ticket, error) {
	b.tmu.Lock()
	b.decided = text
	b.tmu.Unlock()
	return Ticket{ID: id, Status: "planning"}, nil
}

func (b *ticketsBackend) lastDecided() string {
	b.tmu.Lock()
	defer b.tmu.Unlock()
	return b.decided
}

func TestThePlanRoutesAnswerAndRefuse(t *testing.T) {
	srv, b := newTicketsServer(t)
	base := "/api/projects/PRJ-1/tickets/"
	res := api(t, srv, http.MethodPost, base+"TCK-1/plan", "")
	if res.StatusCode != http.StatusCreated || decode[Run](t, res).TicketID != "TCK-1" {
		t.Errorf("plan = %d", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, base+"TCK-2/plan", "")
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "only a top-level ticket") {
		t.Errorf("a refused plan = %d, want 409 with the sentence", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, base+"TCK-1/approve", "")
	if res.StatusCode != http.StatusOK || decode[Ticket](t, res).Status != "ready" {
		t.Errorf("approve = %d", res.StatusCode)
	}
	if res := api(t, srv, http.MethodPost, base+"TCK-3/approve", ""); res.StatusCode != http.StatusConflict {
		t.Errorf("a refused approve = %d, want 409", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, base+"TCK-1/reject", `{"reason":"too big"}`)
	if res.StatusCode != http.StatusOK || b.lastDecided() != "too big" {
		t.Errorf("reject = %d, reason %q", res.StatusCode, b.lastDecided())
	}
	res = api(t, srv, http.MethodPost, base+"TCK-3/reject", `{"reason":"late"}`)
	if res.StatusCode != http.StatusConflict || !strings.Contains(readBody(t, res), "not in review") {
		t.Errorf("a refused reject = %d, want 409 with the sentence", res.StatusCode)
	}
	res = api(t, srv, http.MethodPost, base+"TCK-1/revise", `{"text":"split it"}`)
	if res.StatusCode != http.StatusOK || b.lastDecided() != "split it" {
		t.Errorf("revise = %d, text %q", res.StatusCode, b.lastDecided())
	}
	for _, c := range []struct{ path, body string }{
		{"TCK-1/reject", `{}`},
		{"TCK-1/revise", `{"text":"  "}`},
		{"TCK-1/reject", `{"reason":"x","why":"y"}`},
		{"TCK-1/revise", `{"text":"` + strings.Repeat("a", 16<<10+1) + `"}`},
	} {
		if res := api(t, srv, http.MethodPost, base+c.path, c.body); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s %.40s = %d, want 400", c.path, c.body, res.StatusCode)
		}
	}
	for _, p := range []string{"plan", "approve", "reject", "revise"} {
		if res := api(t, srv, http.MethodGet, base+"TCK-1/"+p, ""); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", p, res.StatusCode)
		}
	}
}
