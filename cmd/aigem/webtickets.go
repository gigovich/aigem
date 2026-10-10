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
		MergePending: v.MergePending,
	}
	for _, c := range v.Comments {
		t.Comments = append(t.Comments, web.TicketComment{At: c.At, By: c.By, Text: c.Text})
	}
	if v.Progress != nil {
		t.Progress = &web.TicketProgress{Done: v.Progress.Done, Total: v.Progress.Total}
	}
	return t
}

func (b *webBackend) RunTicket(ctx context.Context, project, id string) (web.Run, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Run{}, err
	}
	v, err := b.ticketRuns.Start(ctx, project, id)
	if err != nil {
		return web.Run{}, ticketRunError(err)
	}
	return webRun(v), nil
}

func (b *webBackend) MergeTicket(ctx context.Context, project, id string) (web.Ticket, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.ticketRuns.Merge(ctx, project, id)
	if err != nil {
		return web.Ticket{}, ticketRunError(err)
	}
	return webTicket(v), nil
}

func (b *webBackend) Worktrees(ctx context.Context, project string) ([]web.Worktree, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return nil, err
	}
	list, err := b.ticketRuns.Worktrees(ctx, project)
	if err != nil {
		return nil, ticketRunError(err)
	}
	out := make([]web.Worktree, 0, len(list))
	for _, w := range list {
		out = append(out, web.Worktree(w))
	}
	return out, nil
}

func (b *webBackend) DiscardWorktree(ctx context.Context, project, name string) error {
	if err := b.ticketRunsReady(project); err != nil {
		return err
	}
	return ticketRunError(b.ticketRuns.Discard(ctx, project, name))
}

// ticketFinished records a ticket a run left done or blocked in the activity feed.
func (b *webBackend) ticketFinished(_ string, v runner.TicketView, reason string) {
	a := web.Activity{Kind: "ticket.blocked", Text: "Ticket " + v.ID + " blocked: " + firstLine(reason)}
	if v.Status == runner.TicketDone {
		a = web.Activity{Kind: "ticket.done", Text: "Ticket " + v.ID + " done: " + v.Title}
	}
	if n := len(v.Runs); n > 0 {
		a.RunRef = v.Runs[n-1]
	}
	b.recordActivity(a)
}

func (b *webBackend) ticketRunsReady(project string) error {
	if err := b.ticketProject(project); err != nil {
		return err
	}
	if b.ticketRuns == nil {
		return web.ErrUnavailable
	}
	return nil
}

// ticketRunError classifies what the coordinator reports: ticket rules as conflicts, the
// rest the way a run's errors are.
func ticketRunError(err error) error {
	var refusal *runner.TicketRefusal
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runner.ErrNoWorktree):
		return web.ErrNoWorktree
	case errors.As(err, &refusal), errors.Is(err, runner.ErrNoTicket), errors.Is(err, runner.ErrNoProject):
		return webTicketError(err)
	default:
		return webRunError(err)
	}
}

func (b *webBackend) PlanTicket(ctx context.Context, project, id string) (web.Run, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Run{}, err
	}
	v, err := b.ticketRuns.Plan(ctx, project, id)
	if err != nil {
		return web.Run{}, ticketRunError(err)
	}
	return webRun(v), nil
}

func (b *webBackend) ApproveTicket(_ context.Context, project, id string) (web.Ticket, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.ticketRuns.Approve(project, id)
	if err != nil {
		return web.Ticket{}, ticketRunError(err)
	}
	b.recordActivity(web.Activity{Kind: "ticket.approved", Text: "Approved the plan of " + v.ID + ": " + v.Title})
	return webTicket(v), nil
}

func (b *webBackend) RejectTicket(_ context.Context, project, id, reason string) (web.Ticket, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.ticketRuns.Reject(project, id, reason)
	if err != nil {
		return web.Ticket{}, ticketRunError(err)
	}
	b.recordActivity(web.Activity{
		Kind: "ticket.rejected", Text: "Rejected the plan of " + v.ID + ": " + firstLine(reason),
	})
	return webTicket(v), nil
}

func (b *webBackend) ReviseTicket(ctx context.Context, project, id, text string) (web.Ticket, error) {
	if err := b.ticketRunsReady(project); err != nil {
		return web.Ticket{}, err
	}
	v, err := b.ticketRuns.Revise(ctx, project, id, text)
	if err != nil {
		return web.Ticket{}, ticketRunError(err)
	}
	return webTicket(v), nil
}

// ticketPlanned records a plan that reached review in the activity feed.
func (b *webBackend) ticketPlanned(_ string, v runner.TicketView, comment string) {
	a := web.Activity{Kind: "ticket.planned", Text: "Plan of " + v.ID + " in review: " + firstLine(comment)}
	if n := len(v.Runs); n > 0 {
		a.RunRef = v.Runs[n-1]
	}
	b.recordActivity(a)
}
