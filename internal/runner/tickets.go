package runner

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/store"
)

type TicketsConfig struct {
	Dir    string
	Notify func(project string, v TicketView)
	Now    func() time.Time
}

type NewTicket struct {
	Repo, Title, Body, Parent string
	DependsOn                 []string
	By                        string
}

type TicketPatch struct {
	Status    *string
	DependsOn *[]string
}

type Tickets struct {
	dir    string
	notify func(string, TicketView)
	now    func() time.Time

	mu    sync.Mutex
	books map[string]*ticketBook
}

type ticketBook struct {
	file  *store.File[TicketTable]
	table TicketTable
}

func NewTickets(cfg TicketsConfig) *Tickets {
	t := &Tickets{dir: cfg.Dir, notify: cfg.Notify, now: cfg.Now, books: map[string]*ticketBook{}}
	if t.notify == nil {
		t.notify = func(string, TicketView) {}
	}
	if t.now == nil {
		t.now = time.Now
	}
	return t
}

func (t *Tickets) bookLocked(project string) (*ticketBook, error) {
	if b := t.books[project]; b != nil {
		return b, nil
	}
	if n := projectNumber(project); n == 0 || project != projectIDPrefix+strconv.Itoa(n) {
		return nil, ErrNoProject
	}
	b := &ticketBook{}
	if t.dir != "" {
		b.file = store.New[TicketTable](filepath.Join(t.dir, "projects", project, "tickets.json"))
		saved, err := b.file.Load()
		if err != nil {
			return nil, fmt.Errorf("runner: could not read the tickets of %s: %w", project, err)
		}
		b.table = saved
		for _, tk := range saved.Tickets {
			if n, err := strconv.Atoi(strings.TrimPrefix(tk.ID, ticketIDPrefix)); err == nil && n > b.table.Next {
				b.table.Next = n
			}
		}
	}
	t.books[project] = b
	return b, nil
}

func (t *Tickets) List(project string) ([]TicketView, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, err := t.bookLocked(project)
	if err != nil {
		return nil, err
	}
	out := make([]TicketView, 0, len(b.table.Tickets))
	for _, tk := range b.table.Tickets {
		out = append(out, ticketView(b.table.Tickets, tk))
	}
	return out, nil
}

func (t *Tickets) Get(project, id string) (TicketView, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, err := t.bookLocked(project)
	if err != nil {
		return TicketView{}, err
	}
	i := findTicket(b.table.Tickets, id)
	if i < 0 {
		return TicketView{}, ErrNoTicket
	}
	return ticketView(b.table.Tickets, b.table.Tickets[i]), nil
}

func (t *Tickets) Create(project string, n NewTicket) (TicketView, error) {
	title := strings.TrimSpace(n.Title)
	if title == "" {
		return TicketView{}, refuse("a ticket needs a title")
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		if n.Parent != "" {
			i := findTicket(tab.Tickets, n.Parent)
			if i < 0 {
				return nil, refuse("parent %s does not exist", n.Parent)
			}
			if tab.Tickets[i].Parent != "" {
				return nil, refuse("%s is a subticket and cannot have subtickets", n.Parent)
			}
		}
		now := t.now()
		tab.Next++
		tk := Ticket{
			ID: ticketIDPrefix + strconv.Itoa(tab.Next), Repo: n.Repo, Title: title, Body: n.Body,
			Status: TicketOpen, Parent: n.Parent, By: n.By, Created: now, Updated: now,
		}
		deps, err := checkDeps(tab.Tickets, tk, n.DependsOn)
		if err != nil {
			return nil, err
		}
		tk.DependsOn = deps
		tab.Tickets = append(tab.Tickets, tk)
		return []string{tk.ID}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

func (t *Tickets) Update(project, id string, p TicketPatch) (TicketView, error) {
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		if p.DependsOn != nil {
			deps, err := checkDeps(tab.Tickets, *tk, *p.DependsOn)
			if err != nil {
				return nil, err
			}
			tk.DependsOn = deps
		}
		if p.Status != nil && *p.Status != tk.Status {
			switch {
			case !validStatus(*p.Status):
				return nil, refuse("unknown status %q", *p.Status)
			case len(subtickets(tab.Tickets, id)) > 0:
				return nil, refuse("%s follows its subtickets; change them instead", id)
			}
			if err := personMove(tk.Status, *p.Status); err != nil {
				return nil, err
			}
			tk.Status = *p.Status
		}
		tk.Updated = t.now()
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

func (t *Tickets) Comment(project, id, by, text string) (TicketView, error) {
	if strings.TrimSpace(text) == "" {
		return TicketView{}, refuse("a comment cannot be empty")
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		now := t.now()
		tab.Tickets[i].Comments = append(tab.Tickets[i].Comments, Comment{At: now, By: by, Text: text})
		tab.Tickets[i].Updated = now
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

func (t *Tickets) Delete(project, id string) error {
	_, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		if len(subtickets(tab.Tickets, id)) > 0 {
			return nil, refuse("%s has subtickets; close it instead", id)
		}
		for _, other := range tab.Tickets {
			if slices.Contains(other.DependsOn, id) {
				return nil, refuse("%s waits for %s; close it instead", other.ID, id)
			}
		}
		if s := tab.Tickets[i].Status; s == TicketRunning || s == TicketPlanning {
			return nil, refuse("%s is %s; it cannot be deleted now", id, s)
		}
		parent := tab.Tickets[i].Parent
		tab.Tickets = slices.Delete(tab.Tickets, i, i+1)
		if parent != "" {
			return []string{id, parent}, nil
		}
		return []string{id}, nil
	})
	return err
}

// change applies fn to a copy of the project's table, settles the parents it touched, writes
// the copy and keeps it only when the write succeeded. Views come back in the order touched.
func (t *Tickets) change(project string, fn func(*TicketTable) ([]string, error)) ([]TicketView, error) {
	t.mu.Lock()
	b, err := t.bookLocked(project)
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}
	next := cloneTable(b.table)
	touched, err := fn(&next)
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}
	touched = settleParents(&next, touched, t.now())
	if b.file != nil {
		if err := b.file.Save(next); err != nil {
			t.mu.Unlock()
			return nil, fmt.Errorf("runner: could not write the tickets of %s: %w", project, err)
		}
	}
	b.table = next
	views := make([]TicketView, 0, len(touched))
	for _, id := range touched {
		if i := findTicket(next.Tickets, id); i >= 0 {
			views = append(views, ticketView(next.Tickets, next.Tickets[i]))
		} else {
			views = append(views, TicketView{Ticket: Ticket{ID: id}})
		}
	}
	t.mu.Unlock()
	for _, v := range views {
		t.notify(project, v)
	}
	return views, nil
}

// settleParents re-derives the status of every parent of a touched ticket (or a touched parent)
// and returns touched with those parents appended.
func settleParents(tab *TicketTable, touched []string, now time.Time) []string {
	out := slices.Clone(touched)
	for _, id := range touched {
		parent := id
		if i := findTicket(tab.Tickets, id); i >= 0 && tab.Tickets[i].Parent != "" {
			parent = tab.Tickets[i].Parent
		}
		pi := findTicket(tab.Tickets, parent)
		if pi < 0 {
			continue
		}
		kids := subtickets(tab.Tickets, parent)
		if len(kids) == 0 {
			continue
		}
		if s := derive(kids); s != tab.Tickets[pi].Status {
			tab.Tickets[pi].Status = s
			tab.Tickets[pi].Updated = now
		}
		if !slices.Contains(out, parent) {
			out = append(out, parent)
		}
	}
	return out
}

func cloneTable(tab TicketTable) TicketTable {
	out := TicketTable{Next: tab.Next, Tickets: make([]Ticket, len(tab.Tickets))}
	for i, tk := range tab.Tickets {
		tk.DependsOn = slices.Clone(tk.DependsOn)
		tk.Comments = slices.Clone(tk.Comments)
		tk.Runs = slices.Clone(tk.Runs)
		out.Tickets[i] = tk
	}
	return out
}
