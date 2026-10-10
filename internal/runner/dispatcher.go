package runner

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const dispatchEvery = 30 * time.Second

type DispatcherConfig struct {
	Projects   *Projects
	Tickets    *Tickets
	TicketRuns *TicketRuns
	// Started is told about every ticket the dispatcher started, as listed before the start,
	// with its new run.
	Started func(project string, v TicketView, run string)
}

// Dispatcher starts the oldest runnable tickets of every project with free slots. Its passes
// run one at a time on its own goroutine, so Wake is safe from inside the registries' notify.
type Dispatcher struct {
	projects *Projects
	tickets  *Tickets
	runs     *TicketRuns
	started  func(string, TicketView, string)

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
}

func NewDispatcher(cfg DispatcherConfig) *Dispatcher {
	d := newDispatcher(cfg)
	go d.loop()
	return d
}

func newDispatcher(cfg DispatcherConfig) *Dispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		projects: cfg.Projects, tickets: cfg.Tickets, runs: cfg.TicketRuns, started: cfg.Started,
		ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
}

// Wake asks for a pass. Wake-ups that arrive during a pass cause one more.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Close cancels the start in flight and waits for the dispatcher to stop.
func (d *Dispatcher) Close() {
	d.cancel()
	<-d.done
}

func (d *Dispatcher) loop() {
	defer close(d.done)
	tick := time.NewTicker(dispatchEvery)
	defer tick.Stop()
	for d.pass() {
		select {
		case <-d.ctx.Done():
			return
		case <-d.wake:
		case <-tick.C:
		}
	}
}

// pass fills the free slots of every project and reports whether the dispatcher goes on.
func (d *Dispatcher) pass() bool {
	for _, p := range d.projects.List() {
		if p.Slots == 0 || p.Paused {
			continue
		}
		views, err := d.tickets.List(p.ID)
		if err != nil {
			slog.Warn("the dispatcher could not read a project's tickets", "project", p.ID, "err", err)
			continue
		}
		running := 0
		for _, v := range views {
			if v.Status == TicketRunning && v.Progress == nil {
				running++
			}
		}
		for _, v := range views {
			if running >= p.Slots {
				break
			}
			if !v.Runnable {
				continue
			}
			run, err := d.runs.Start(d.ctx, p.ID, v.ID)
			switch {
			case err == nil:
				running++
				d.started(p.ID, v, run.ID)
			case errors.Is(err, ErrRunsClosed), d.ctx.Err() != nil:
				return false
			}
		}
	}
	return true
}
