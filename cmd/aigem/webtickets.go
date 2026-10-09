package main

import (
	"context"
	"errors"

	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/web"
)

func (b *webBackend) Tickets(_ context.Context, project string) ([]web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return nil, err
	}
	views, err := b.tickets.List(project)
	if err != nil {
		return nil, webTicketError(err)
	}
	out := make([]web.Ticket, 0, len(views))
	for _, v := range views {
		out = append(out, webTicket(v))
	}
	return out, nil
}

func (b *webBackend) Ticket(_ context.Context, project, id string) (web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.tickets.Get(project, id)
	return webTicket(v), webTicketError(err)
}

func (b *webBackend) CreateTicket(_ context.Context, project string, req web.NewTicket) (web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.tickets.Create(project, runner.NewTicket{
		Repo: req.Repo, Title: req.Title, Body: req.Body, Parent: req.Parent,
		DependsOn: req.DependsOn, By: "you",
	})
	if err != nil {
		return web.Ticket{}, webTicketError(err)
	}
	b.recordActivity(web.Activity{Kind: "ticket.created", Text: "Created ticket " + v.ID + ": " + v.Title})
	return webTicket(v), nil
}

func (b *webBackend) UpdateTicket(_ context.Context, project, id string, req web.TicketPatch) (web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return web.Ticket{}, err
	}
	before, _ := b.tickets.Get(project, id)
	v, err := b.tickets.Update(project, id, runner.TicketPatch{Status: req.Status, DependsOn: req.DependsOn})
	if err != nil {
		return web.Ticket{}, webTicketError(err)
	}
	if v.Status == runner.TicketClosed && before.Status != runner.TicketClosed {
		b.recordActivity(web.Activity{Kind: "ticket.closed", Text: "Closed ticket " + v.ID + ": " + v.Title})
	}
	return webTicket(v), nil
}

func (b *webBackend) CommentTicket(_ context.Context, project, id, text string) (web.Ticket, error) {
	if err := b.ticketProject(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.tickets.Comment(project, id, "you", text)
	return webTicket(v), webTicketError(err)
}

func (b *webBackend) DeleteTicket(_ context.Context, project, id string) error {
	if err := b.ticketProject(project); err != nil {
		return err
	}
	return webTicketError(b.tickets.Delete(project, id))
}

// ticketProject answers for a project the registry no longer lists, before the tickets
// registry would happily open a file for it.
func (b *webBackend) ticketProject(project string) error {
	if b.tickets == nil || b.projects == nil {
		return web.ErrUnavailable
	}
	if _, err := b.projects.Get(project); err != nil {
		return webProjectError(err)
	}
	return nil
}

func webTicketError(err error) error {
	var refusal *runner.TicketRefusal
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runner.ErrNoTicket):
		return web.ErrNoTicket
	case errors.Is(err, runner.ErrNoProject):
		return web.ErrNoProject
	case errors.As(err, &refusal):
		return web.Conflict(refusal.Reason)
	default:
		return err
	}
}

func webTicket(v runner.TicketView) web.Ticket {
	t := web.Ticket{
		ID: v.ID, Repo: v.Repo, Title: v.Title, Body: v.Body, Status: v.Status, Parent: v.Parent,
		DependsOn: append([]string{}, v.DependsOn...), By: v.By, Created: v.Created, Updated: v.Updated,
		Comments: []web.TicketComment{}, Runs: append([]string{}, v.Runs...), Runnable: v.Runnable,
	}
	for _, c := range v.Comments {
		t.Comments = append(t.Comments, web.TicketComment{At: c.At, By: c.By, Text: c.Text})
	}
	if v.Progress != nil {
		t.Progress = &web.TicketProgress{Done: v.Progress.Done, Total: v.Progress.Total}
	}
	return t
}
