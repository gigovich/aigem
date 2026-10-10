package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// planTool is one ticket tool of a planner run.
type planTool struct {
	name, desc, schema string
	run                func(ctx context.Context, args json.RawMessage) (string, error)
}

func (p *planTool) Name() string            { return p.name }
func (p *planTool) Description() string     { return p.desc }
func (p *planTool) Schema() json.RawMessage { return json.RawMessage(p.schema) }
func (*planTool) NeedsConfirm() bool        { return false }

func (p *planTool) Run(ctx context.Context, args json.RawMessage) (string, error) {
	return p.run(ctx, args)
}

// planTicket is a ticket as a planner reads it.
type planTicket struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo,omitempty"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Parent    string    `json:"parent,omitempty"`
	DependsOn []string  `json:"dependsOn,omitempty"`
	Body      string    `json:"body,omitempty"`
	Comments  []Comment `json:"comments,omitempty"`
}

const (
	idSchema   = `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`
	depsSchema = `{"type":"array","items":{"type":"string"},"description":"ids of the tickets it waits for"}`
)

// planTools are a planner run's ticket tools. Each acts only while the run's ticket is planning
// under this run, and every change goes through Tickets with the run's id, so the rules hold.
func (t *TicketRuns) planTools(tr *ticketRun) []tools.Tool {
	return []tools.Tool{
		&planTool{"list_tickets", "List the project's tickets: id, repo, title, status, parent and dependencies.",
			`{"type":"object","properties":{}}`,
			func(context.Context, json.RawMessage) (string, error) {
				if err := t.planning(tr); err != nil {
					return "", err
				}
				views, err := t.tickets.List(tr.project)
				if err != nil {
					return "", err
				}
				out := make([]planTicket, 0, len(views))
				for _, v := range views {
					out = append(out, planTicket{ID: v.ID, Repo: v.Repo, Title: v.Title, Status: v.Status,
						Parent: v.Parent, DependsOn: v.DependsOn})
				}
				return jsonText(out)
			}},
		&planTool{"get_ticket", "Read one ticket of the project with its body and discussion.", idSchema,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if err := t.planning(tr); err != nil {
					return "", err
				}
				v, err := t.tickets.Get(tr.project, in.ID)
				if err != nil {
					return "", err
				}
				return jsonText(planTicket{ID: v.ID, Repo: v.Repo, Title: v.Title, Status: v.Status,
					Parent: v.Parent, DependsOn: v.DependsOn, Body: v.Body, Comments: v.Comments})
			}},
		&planTool{"create_subticket", "Create a subticket of the ticket you plan. repo is one of the " +
			`project's repositories, "" for the project directory; dependsOn lists the tickets it waits for.`,
			`{"type":"object","properties":{"repo":{"type":"string"},"title":{"type":"string"},` +
				`"body":{"type":"string"},"dependsOn":` + depsSchema + `},"required":["title"]}`,
			func(ctx context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Repo      string   `json:"repo"`
					Title     string   `json:"title"`
					Body      string   `json:"body"`
					DependsOn []string `json:"dependsOn"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if err := t.planRepo(ctx, tr.project, in.Repo); err != nil {
					return "", err
				}
				v, err := t.tickets.create(tr.project, NewTicket{Repo: in.Repo, Title: in.Title, Body: in.Body,
					Parent: tr.ticket, DependsOn: in.DependsOn, By: "run " + tr.run}, tr.run)
				if err != nil {
					return "", err
				}
				return "Created " + v.ID + ".", nil
			}},
		&planTool{"delete_subticket", "Delete a subticket of the ticket you plan.", idSchema,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if err := t.tickets.remove(tr.project, in.ID, tr.run); err != nil {
					return "", err
				}
				return "Deleted " + in.ID + ".", nil
			}},
		&planTool{"set_dependencies", "Replace the tickets a subticket of the ticket you plan waits for.",
			`{"type":"object","properties":{"id":{"type":"string"},"dependsOn":` + depsSchema +
				`},"required":["id","dependsOn"]}`,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					ID        string   `json:"id"`
					DependsOn []string `json:"dependsOn"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				v, err := t.tickets.update(tr.project, in.ID, TicketPatch{DependsOn: &in.DependsOn}, tr.run)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%s waits for [%s].", v.ID, strings.Join(v.DependsOn, ", ")), nil
			}},
		&planTool{"plan_done", "Call this once the plan is complete, with a short summary for the person " +
			"who reviews it. Do not call it while you still need an answer.",
			`{"type":"object","properties":{"summary":{"type":"string",` +
				`"description":"The plan in a few sentences."}},"required":["summary"]}`,
			func(_ context.Context, args json.RawMessage) (string, error) {
				var in struct {
					Summary string `json:"summary"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return "", err
				}
				if err := t.planning(tr); err != nil {
					return "", err
				}
				summary := strings.TrimSpace(in.Summary)
				if summary == "" {
					return "", errors.New("a summary is required")
				}
				tr.record(summary)
				return "Recorded. End your turn now; a person reviews the plan.", nil
			}},
	}
}

// planning refuses a planner tool call unless the run's ticket is planning under this run.
func (t *TicketRuns) planning(tr *ticketRun) error {
	v, err := t.tickets.Get(tr.project, tr.ticket)
	if err != nil {
		return err
	}
	return planLock([]Ticket{v.Ticket}, tr.ticket, tr.run)
}

// planRepo refuses a repository that is not one of the project's checkouts; "" is the project
// directory itself.
func (t *TicketRuns) planRepo(ctx context.Context, project, repo string) error {
	if repo == "" {
		return nil
	}
	repos, err := t.projects.Repositories(ctx, project)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(repos, func(r Repository) bool { return r.Name == repo }) {
		return refuse("%q is not a repository of this project", repo)
	}
	return nil
}

func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
