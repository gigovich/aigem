package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// The tickets API: a project's tickets, their subtickets and dependencies. Every rule lives
// behind the seam; a refused rule comes back as a Conflict carrying the sentence to show.

type TicketsBackend interface {
	Tickets(ctx context.Context, project string) ([]Ticket, error)
	Ticket(ctx context.Context, project, id string) (Ticket, error)
	CreateTicket(ctx context.Context, project string, req NewTicket) (Ticket, error)
	UpdateTicket(ctx context.Context, project, id string, req TicketPatch) (Ticket, error)
	CommentTicket(ctx context.Context, project, id, text string) (Ticket, error)
	DeleteTicket(ctx context.Context, project, id string) error
}

type Ticket struct {
	ID        string          `json:"id"`
	Repo      string          `json:"repo"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Status    string          `json:"status"`
	Parent    string          `json:"parent,omitempty"`
	DependsOn []string        `json:"dependsOn"`
	By        string          `json:"by"`
	Created   time.Time       `json:"created,omitzero"`
	Updated   time.Time       `json:"updated,omitzero"`
	Comments  []TicketComment `json:"comments"`
	Runs      []string        `json:"runs"`
	Runnable  bool            `json:"runnable"`
	Progress  *TicketProgress `json:"progress,omitempty"`
}

type TicketComment struct {
	At   time.Time `json:"at"`
	By   string    `json:"by"`
	Text string    `json:"text"`
}

type TicketProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

type NewTicket struct {
	Repo      string   `json:"repo,omitempty"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Parent    string   `json:"parent,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

type TicketPatch struct {
	Status    *string   `json:"status,omitempty"`
	DependsOn *[]string `json:"dependsOn,omitempty"`
}

// ErrNoTicket is returned for a ticket id the project does not hold.
var ErrNoTicket = errors.New("web: no such ticket")

const (
	maxTicketBody   = 64 << 10
	maxCommentBytes = 16 << 10
)

func (s *Server) handleTickets(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	items, err := b.Tickets(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRunError(w, "listing tickets", err)
		return
	}
	status, parent := r.URL.Query().Get("status"), r.URL.Query().Get("parent")
	out := []Ticket{}
	for _, t := range items {
		if (status == "" || t.Status == status) && (parent == "" || t.Parent == parent) {
			out = append(out, t)
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	t, err := b.Ticket(r.Context(), r.PathValue("id"), r.PathValue("tid"))
	if err != nil {
		writeRunError(w, "reading a ticket", err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	var req NewTicket
	if err := decodeJSONLimit(w, r, &req, maxTicketBody); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	t, err := b.CreateTicket(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeRunError(w, "creating a ticket", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, t)
}

func (s *Server) handleUpdateTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	var req TicketPatch
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	t, err := b.UpdateTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"), req)
	if err != nil {
		writeRunError(w, "changing a ticket", err)
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleCommentTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeJSONLimit(w, r, &req, maxCommentBytes+1<<10); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Text) > maxCommentBytes {
		http.Error(w, "a comment is at most 16 KiB", http.StatusBadRequest)
		return
	}
	t, err := b.CommentTicket(r.Context(), r.PathValue("id"), r.PathValue("tid"), req.Text)
	if err != nil {
		writeRunError(w, "commenting on a ticket", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, t)
}

func (s *Server) handleDeleteTicket(w http.ResponseWriter, r *http.Request) {
	b, ok := backendOf[TicketsBackend](s, w, "tickets")
	if !ok {
		return
	}
	if err := b.DeleteTicket(r.Context(), r.PathValue("id"), r.PathValue("tid")); err != nil {
		writeRunError(w, "deleting a ticket", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
