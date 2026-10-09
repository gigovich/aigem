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
	if res := api(t, srv, http.MethodGet, "/api/projects/PRJ-1/tickets/TCK-7", ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown ticket = %d, want 404", res.StatusCode)
	}

	res := api(t, srv, http.MethodPost, "/api/projects/PRJ-1/tickets", `{"title":"new","dependsOn":["TCK-3"]}`)
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

	if res := api(t, srv, http.MethodDelete, "/api/projects/PRJ-1/tickets/TCK-3", ""); res.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", res.StatusCode)
	}
	if res := api(t, srv, http.MethodDelete, "/api/projects/PRJ-1/tickets/TCK-1", ""); res.StatusCode != http.StatusConflict {
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
	if res := api(t, projectsOnly, http.MethodGet, "/api/projects/PRJ-1/tickets", ""); res.StatusCode != http.StatusNotImplemented {
		t.Errorf("a backend without the seam = %d, want 501", res.StatusCode)
	}
}
