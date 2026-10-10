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
	return t.create(project, n, "")
}

// create adds a ticket for a person (run "") or for the planner run that plans its parent.
func (t *Tickets) create(project string, n NewTicket, run string) (TicketView, error) {
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
		if err := planLock(tab.Tickets, n.Parent, run); err != nil {
			return nil, err
		}
		now := t.now()
		tab.Next++
		tk := Ticket{
			ID: ticketIDPrefix + strconv.Itoa(tab.Next), Repo: n.Repo, Title: title, Body: n.Body,
			Status: TicketOpen, Parent: n.Parent, By: n.By, Created: now, Updated: now,
		}
		tab.Tickets = append(tab.Tickets, tk)
		deps, err := checkDeps(tab.Tickets, tk, n.DependsOn)
		if err != nil {
			return nil, err
		}
		tab.Tickets[len(tab.Tickets)-1].DependsOn = deps
		return []string{tk.ID}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

func (t *Tickets) Update(project, id string, p TicketPatch) (TicketView, error) {
	return t.update(project, id, p, "")
}

// update changes a ticket for a person (run "") or relinks a draft for its planner run.
func (t *Tickets) update(project, id string, p TicketPatch, run string) (TicketView, error) {
	if run != "" && p.Status != nil {
		return TicketView{}, refuse("a planner run cannot change a status")
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		changed := false
		if p.DependsOn != nil {
			if err := planLock(tab.Tickets, tk.Parent, run); err != nil {
				return nil, err
			}
			deps, err := checkDeps(tab.Tickets, *tk, *p.DependsOn)
			if err != nil {
				return nil, err
			}
			changed = !slices.Equal(deps, tk.DependsOn)
			tk.DependsOn = deps
		}
		if p.Status != nil && *p.Status != tk.Status {
			switch {
			case !validStatus(*p.Status):
				return nil, refuse("unknown status %q", *p.Status)
			case isDraft(tab.Tickets, *tk):
				return nil, refuse("%s is part of a plan in review", id)
			case len(subtickets(tab.Tickets, id)) > 0:
				return nil, refuse("%s follows its subtickets; change them instead", id)
			}
			if err := personMove(tk.Status, *p.Status); err != nil {
				return nil, err
			}
			tk.Status = *p.Status
			changed = true
		}
		if !changed {
			return nil, nil
		}
		tk.Updated = t.now()
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	if len(views) == 0 {
		return t.Get(project, id)
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

// maxRunComment caps what a run writes into a ticket's discussion.
const maxRunComment = 16 << 10

func lastRun(tk Ticket, run string) bool {
	return len(tk.Runs) > 0 && tk.Runs[len(tk.Runs)-1] == run
}

func clipRunComment(s string) string {
	if len(s) > maxRunComment {
		return strings.ToValidUTF8(s[:maxRunComment], "") + "…"
	}
	return s
}

// Start hands a ticket to a run. A new run needs a runnable ticket; the run that already
// drives it (its last) takes it back from blocked when a person typed into it.
func (t *Tickets) Start(project, id, run string) (TicketView, error) {
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		owns := lastRun(*tk, run)
		switch {
		case !owns && slices.Contains(tk.Runs, run):
			return nil, refuse("%s is driven by another run", id)
		case owns && tk.Status == TicketRunning:
			return nil, nil
		case owns && tk.Status == TicketBlocked:
		case tk.Status == TicketRunning:
			return nil, refuse("%s is already running", id)
		case !ticketView(tab.Tickets, *tk).Runnable:
			return nil, refuse("%s is not runnable", id)
		}
		if !owns {
			tk.Runs = append(tk.Runs, run)
		}
		tk.Status, tk.MergePending, tk.Updated = TicketRunning, false, t.now()
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	if len(views) == 0 {
		return t.Get(project, id)
	}
	return views[0], nil
}

// StartPlan hands an open top-level ticket without subtickets to a new planner run. The run
// that already plans it (its last) takes it back from review when a person typed into it.
func (t *Tickets) StartPlan(project, id, run string) (TicketView, error) {
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		owns := lastRun(*tk, run)
		switch {
		case owns && tk.Status == TicketPlanning:
			return nil, nil
		case owns && tk.Status != TicketReview:
			return nil, refuse("%s is %s, not in review", id, tk.Status)
		case !owns && slices.Contains(tk.Runs, run):
			return nil, refuse("%s is driven by another run", id)
		case !owns:
			if err := plannable(*tk, len(subtickets(tab.Tickets, id)) > 0); err != nil {
				return nil, err
			}
			tk.Runs = append(tk.Runs, run)
		}
		tk.Status, tk.Updated = TicketPlanning, t.now()
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	if len(views) == 0 {
		return t.Get(project, id)
	}
	return views[0], nil
}

// PlanReview hands a plan to a person: the ticket goes to review with the planner's comment.
// Only the ticket's last run may do it.
func (t *Tickets) PlanReview(project, id, run, comment string) (TicketView, error) {
	comment = clipRunComment(comment)
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		switch {
		case !lastRun(*tk, run):
			return nil, refuse("%s is driven by another run", id)
		case tk.Status != TicketPlanning && tk.Status != TicketReview:
			return nil, refuse("%s is %s; no plan is in progress", id, tk.Status)
		}
		now := t.now()
		tk.Status, tk.Updated = TicketReview, now
		if comment != "" {
			tk.Comments = append(tk.Comments, Comment{At: now, By: "aigem", Text: comment})
		}
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

func inReview(rows []Ticket, id string) (int, error) {
	i := findTicket(rows, id)
	switch {
	case i < 0:
		return -1, ErrNoTicket
	case rows[i].Status != TicketReview:
		return -1, refuse("%s is %s, not in review", id, rows[i].Status)
	}
	return i, nil
}

// Approve makes a reviewed plan's subtickets ready and hands its parent back to them.
func (t *Tickets) Approve(project, id string) (TicketView, error) {
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i, err := inReview(tab.Tickets, id)
		if err != nil {
			return nil, err
		}
		now := t.now()
		touched := []string{id}
		for k := range tab.Tickets {
			if tab.Tickets[k].Parent == id {
				tab.Tickets[k].Status, tab.Tickets[k].Updated = TicketReady, now
				touched = append(touched, tab.Tickets[k].ID)
			}
		}
		if len(touched) == 1 {
			return nil, refuse("%s's plan has no subtickets; revise or reject it", id)
		}
		tab.Tickets[i].Status, tab.Tickets[i].Updated = derive(subtickets(tab.Tickets, id)), now
		return touched, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

// Reject deletes a reviewed plan's subtickets and opens its ticket again with the reason.
func (t *Tickets) Reject(project, id, reason string) (TicketView, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return TicketView{}, refuse("a rejection needs a reason")
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		if _, err := inReview(tab.Tickets, id); err != nil {
			return nil, err
		}
		touched := []string{id}
		tab.Tickets = slices.DeleteFunc(tab.Tickets, func(k Ticket) bool {
			if k.Parent == id {
				touched = append(touched, k.ID)
			}
			return k.Parent == id
		})
		now := t.now()
		tk := &tab.Tickets[findTicket(tab.Tickets, id)]
		tk.Status, tk.Updated = TicketOpen, now
		tk.Comments = append(tk.Comments, Comment{At: now, By: "you", Text: "Plan rejected: " + reason})
		return touched, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

// Revise sends a reviewed plan back to planning under run, with the person's feedback.
func (t *Tickets) Revise(project, id, run, feedback string) (TicketView, error) {
	if strings.TrimSpace(feedback) == "" {
		return TicketView{}, refuse("feedback cannot be empty")
	}
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i, err := inReview(tab.Tickets, id)
		if err != nil {
			return nil, err
		}
		tk := &tab.Tickets[i]
		if !lastRun(*tk, run) {
			tk.Runs = append(tk.Runs, run)
		}
		now := t.now()
		tk.Status, tk.Updated = TicketPlanning, now
		tk.Comments = append(tk.Comments, Comment{At: now, By: "you", Text: feedback})
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

// Finish records how a run left a ticket: done, or blocked with the reason as a comment.
// Only the ticket's last run may do it.
func (t *Tickets) Finish(project, id, run, status, comment string, mergePending bool) (TicketView, error) {
	if status != TicketDone && status != TicketBlocked {
		return TicketView{}, refuse("a run cannot leave a ticket %s", status)
	}
	comment = clipRunComment(comment)
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		if !lastRun(*tk, run) {
			return nil, refuse("%s is driven by another run", id)
		}
		if tk.Status != TicketRunning && tk.Status != TicketBlocked {
			return nil, refuse("%s is %s; no run drives it", id, tk.Status)
		}
		now := t.now()
		tk.Status, tk.MergePending, tk.Updated = status, mergePending && status == TicketBlocked, now
		if comment != "" {
			tk.Comments = append(tk.Comments, Comment{At: now, By: "aigem", Text: comment})
		}
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

// Block moves a ready ticket that could not start to blocked, with the reason as a comment.
func (t *Tickets) Block(project, id, reason string) (TicketView, error) {
	reason = clipRunComment(reason)
	views, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		tk := &tab.Tickets[i]
		switch {
		case tk.Status != TicketReady:
			return nil, refuse("%s is %s, not ready", id, tk.Status)
		case !ticketView(tab.Tickets, *tk).Runnable:
			return nil, refuse("%s is not runnable", id)
		}
		now := t.now()
		tk.Status, tk.Updated = TicketBlocked, now
		tk.Comments = append(tk.Comments, Comment{At: now, By: "aigem", Text: reason})
		return []string{id}, nil
	})
	if err != nil {
		return TicketView{}, err
	}
	return views[0], nil
}

func (t *Tickets) Delete(project, id string) error { return t.remove(project, id, "") }

// remove deletes a ticket for a person (run "") or a draft for the planner run that plans it.
func (t *Tickets) remove(project, id, run string) error {
	_, err := t.change(project, func(tab *TicketTable) ([]string, error) {
		i := findTicket(tab.Tickets, id)
		if i < 0 {
			return nil, ErrNoTicket
		}
		if err := planLock(tab.Tickets, tab.Tickets[i].Parent, run); err != nil {
			return nil, err
		}
		if len(subtickets(tab.Tickets, id)) > 0 {
			return nil, refuse("%s has subtickets; close it instead", id)
		}
		for _, other := range tab.Tickets {
			if slices.Contains(other.DependsOn, id) {
				return nil, refuse("%s waits for %s; close it instead", other.ID, id)
			}
		}
		if s := tab.Tickets[i].Status; s == TicketRunning || s == TicketPlanning || s == TicketReview {
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
// the copy and keeps it only when the write succeeded. Views come back in the order touched;
// when fn touches nothing, nothing is written or announced.
func (t *Tickets) change(project string, fn func(*TicketTable) ([]string, error)) ([]TicketView, error) {
	t.mu.Lock()
	b, err := t.bookLocked(project)
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}
	next := cloneTable(b.table)
	touched, err := fn(&next)
	if err != nil || len(touched) == 0 {
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
		// A plan in progress or in review does not follow its draft.
		held := tab.Tickets[pi].Status == TicketPlanning || tab.Tickets[pi].Status == TicketReview
		if s := derive(kids); !held && s != tab.Tickets[pi].Status {
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
