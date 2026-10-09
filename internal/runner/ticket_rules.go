package runner

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	TicketOpen     = "open"
	TicketPlanning = "planning"
	TicketReview   = "review"
	TicketReady    = "ready"
	TicketRunning  = "running"
	TicketBlocked  = "blocked"
	TicketDone     = "done"
	TicketClosed   = "closed"
)

const ticketIDPrefix = "TCK-"

var ErrNoTicket = errors.New("runner: no such ticket")

// TicketRefusal is a change the ticket rules refuse. Reason is written for a person.
type TicketRefusal struct{ Reason string }

func (e *TicketRefusal) Error() string { return e.Reason }

func refuse(format string, a ...any) error {
	return &TicketRefusal{Reason: fmt.Sprintf(format, a...)}
}

type Comment struct {
	At   time.Time `json:"at"`
	By   string    `json:"by"`
	Text string    `json:"text"`
}

type Ticket struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Status    string    `json:"status"`
	Parent    string    `json:"parent,omitempty"`
	DependsOn []string  `json:"dependsOn,omitempty"`
	By        string    `json:"by"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	Comments  []Comment `json:"comments,omitempty"`
	Runs      []string  `json:"runs,omitempty"`
	// MergePending marks a blocked ticket whose branch is committed and checked and only waits
	// for the merge into main.
	MergePending bool `json:"mergePending,omitempty"`
}

// TicketTable is one project's saved tickets. Next survives a delete, so an id never comes back.
type TicketTable struct {
	Next    int      `json:"next"`
	Tickets []Ticket `json:"tickets"`
}

type TicketProgress struct{ Done, Total int }

type TicketView struct {
	Ticket
	Runnable bool
	Progress *TicketProgress
}

func validStatus(s string) bool {
	switch s {
	case TicketOpen, TicketPlanning, TicketReview, TicketReady, TicketRunning, TicketBlocked,
		TicketDone, TicketClosed:
		return true
	}
	return false
}

func findTicket(rows []Ticket, id string) int {
	return slices.IndexFunc(rows, func(t Ticket) bool { return t.ID == id })
}

func subtickets(rows []Ticket, id string) []Ticket {
	var out []Ticket
	for _, t := range rows {
		if t.Parent == id {
			out = append(out, t)
		}
	}
	return out
}

func personMove(from, to string) error {
	switch {
	case to == TicketClosed && from != TicketRunning && from != TicketPlanning && from != TicketReview,
		from == TicketOpen && to == TicketReady,
		from == TicketReady && to == TicketOpen,
		from == TicketBlocked && (to == TicketOpen || to == TicketReady),
		from == TicketClosed && to == TicketOpen,
		(from == TicketReady || from == TicketBlocked) && to == TicketDone:
		return nil
	}
	return refuse("a ticket cannot move from %s to %s", from, to)
}

func derive(kids []Ticket) string {
	count := map[string]int{}
	for _, k := range kids {
		count[k.Status]++
	}
	switch {
	case count[TicketDone] > 0 && count[TicketDone]+count[TicketClosed] == len(kids):
		return TicketDone
	case count[TicketClosed] == len(kids):
		return TicketClosed
	case count[TicketBlocked] > 0:
		return TicketBlocked
	case count[TicketRunning] > 0:
		return TicketRunning
	case count[TicketReady] > 0:
		return TicketReady
	}
	return TicketOpen
}

// checkDeps validates the tickets t would wait for and returns them without duplicates.
func checkDeps(rows []Ticket, t Ticket, deps []string) ([]string, error) {
	var out []string
	for _, d := range deps {
		switch i := findTicket(rows, d); {
		case d == t.ID:
			return nil, refuse("%s cannot wait for itself", d)
		case i < 0:
			return nil, refuse("%s does not exist", d)
		case d == t.Parent:
			return nil, refuse("%s cannot wait for its own parent %s", t.ID, d)
		case rows[i].Parent == t.ID:
			return nil, refuse("%s cannot wait for its own subticket %s", t.ID, d)
		}
		if slices.Contains(out, d) {
			continue
		}
		// A parent's dependencies also hold its subtickets back, so a wait back to one of them is a cycle too.
		for _, target := range append([]Ticket{t}, subtickets(rows, t.ID)...) {
			if path := waitPath(rows, d, target.ID, map[string]bool{}); path != nil {
				return nil, refuse("that makes a cycle: %s -> %s", t.ID, strings.Join(path, " -> "))
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// waitsOn is what t waits for: its own dependencies and those of its parent.
func waitsOn(rows []Ticket, t Ticket) []string {
	out := slices.Clone(t.DependsOn)
	if p := findTicket(rows, t.Parent); p >= 0 {
		out = append(out, rows[p].DependsOn...)
	}
	return out
}

// waitPath is the chain of waits from one ticket to another, or nil when there is none.
// A parent waits for its subtickets, and a subticket for its parent's dependencies.
func waitPath(rows []Ticket, from, to string, seen map[string]bool) []string {
	if from == to {
		return []string{to}
	}
	if seen[from] {
		return nil
	}
	seen[from] = true
	i := findTicket(rows, from)
	if i < 0 {
		return nil
	}
	edges := waitsOn(rows, rows[i])
	for _, k := range subtickets(rows, from) {
		edges = append(edges, k.ID)
	}
	for _, next := range edges {
		if rest := waitPath(rows, next, to, seen); rest != nil {
			return append([]string{from}, rest...)
		}
	}
	return nil
}

func ticketView(rows []Ticket, t Ticket) TicketView {
	v := TicketView{Ticket: t}
	if kids := subtickets(rows, t.ID); len(kids) > 0 {
		done := 0
		for _, k := range kids {
			if k.Status == TicketDone {
				done++
			}
		}
		v.Progress = &TicketProgress{Done: done, Total: len(kids)}
		return v
	}
	if t.Status != TicketReady {
		return v
	}
	for _, d := range waitsOn(rows, t) {
		if i := findTicket(rows, d); i < 0 || rows[i].Status != TicketDone {
			return v
		}
	}
	v.Runnable = true
	return v
}
