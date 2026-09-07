package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/store"
	"github.com/gigovich/aigem/internal/web"
)

// The daemon's half of a run: how one conversation is assembled from the
// environment the daemon loaded once.
//
// It is here rather than in internal/runner because everything in it is a
// choice this binary makes - which model to open, what a turn costs, what the
// system prompt is built from - and putting it behind the registry would mean
// internal/llm and internal/auth behind every test of the table.

// The defaults a browser session runs with. They are the flag defaults the
// terminal uses, stated again rather than shared, because `aigem web` takes no
// flags for them yet and a daemon that silently ran at a different temperature
// than the terminal would be a difference nobody could see.
const (
	webTemp         = 0.3
	webCompactAt    = 70
	webEvictAt      = 50
	webKeepTurns    = 10
	webKeepTools    = 4
	webCompactAuto  = true
	webRunTableName = "runs.json"
)

// webRuntime is what the daemon holds for the whole of its life: one
// environment for the directory it was started in, and the model registry the
// operator configured.
//
// One environment, not one per run: the skills, the subagents, the hooks
// configuration and the MCP servers belong to the project, and a second copy of
// them would be a second set of stdio servers for the same project. What a run
// gets of its own is the tools registry and the model handle, which is exactly
// what internal/runner says must never be shared.
type webRuntime struct {
	env    *runner.Env
	models *llm.Registry
}

// newRuns builds the daemon's run table. stateDir is where the table is kept;
// an empty one keeps the runs in memory, which is what a daemon that could not
// find its state directory falls back to.
func (rt *webRuntime) newRuns(stateDir string, notify func(runner.RunView)) (*runner.Runs, error) {
	cfg := runner.RunsConfig{Open: rt.openRun, Notify: notify}
	if stateDir != "" {
		cfg.Store = store.New[[]runner.Run](filepath.Join(stateDir, webRunTableName))
	}
	return runner.NewRuns(cfg)
}

// openRun assembles one conversation.
//
// It is the browser's equivalent of what main does for the terminal, and it is
// deliberately the same shape: the same runner.Spec, built by the same package,
// so the two front-ends cannot drift into holding conversations that behave
// differently.
func (rt *webRuntime) openRun(_ context.Context, req runner.RunRequest) (
	*runner.Session, runner.Opened, error,
) {
	// A registry belongs to one conversation: it carries the session's approval
	// function and its path grants, and sharing one would route a second
	// person's question to the first person's browser.
	reg, err := rt.env.NewTools()
	if err != nil {
		return nil, runner.Opened{}, err
	}
	// Directories the person has already approved for this project are read
	// without asking again; everything else is asked about on the socket.
	reg.SetPathGrants(true)

	ref := req.Model
	if ref == "" {
		ref = preferredModelRef(rt.models)
	}
	if ref == "" {
		return nil, runner.Opened{}, errors.New("no model is signed in: run " +
			"`aigem auth login <provider>`, or point the daemon at a local model")
	}
	backend, _, info, err := auth.OpenModel(rt.models, ref, defaultMaxTokens)
	if err != nil {
		return nil, runner.Opened{}, fmt.Errorf("could not open %s: %w", ref, err)
	}
	ctxSize := info.ContextWindow
	if ctxSize <= 0 {
		ctxSize = runner.DefaultCtxSize
	}

	// Rebuilt rather than captured, so that an edit to AGENTS.md or CLAUDE.md
	// takes effect on the next fresh conversation without a restart. It marks
	// the injected files in this run's own registry, so read_file returns a
	// note instead of re-emitting what is already in the prompt.
	buildSystem := func() string {
		sp, injected := rt.env.SystemPrompt()
		reg.MarkInContext(injected)
		return sp
	}
	title := req.Title
	if title == "" {
		title = rt.env.SessionTitle
	}
	backendRef := webModelRef(backend)
	sess := runner.NewSession(runner.Spec{
		Mode:    req.Mode,
		Tools:   reg,
		Backend: backendRef,
		Models:  rt.models,
		Agents:  rt.env.Agents,
		Skills:  rt.env.Skills,
		Hooks:   rt.env.Hooks,
		Project: rt.env.Project,
		System:  buildSystem(),
		Title:   title,
		// A conversation started in a browser runs the person's own hooks, and
		// a hook that is told nothing about where it is running would resolve
		// paths against wherever the daemon happens to have been started.
		Cwd:           rt.env.Cwd,
		RebuildSystem: buildSystem,
		Temp:          webTemp,
		MaxTokens:     defaultMaxTokens,
		CtxSize:       ctxSize,
		Compact: agent.CompactConfig{
			Auto:         webCompactAuto,
			CtxSize:      ctxSize,
			CompactAtPct: webCompactAt,
			EvictAtPct:   webEvictAt,
			KeepTurns:    webKeepTurns,
			KeepTools:    webKeepTools,
		},
	})
	// Approving the project's skills has to reach every live conversation, not
	// just the one that asked.
	if err := rt.env.Attach(sess); err != nil {
		sess.Local.Close()
		return nil, runner.Opened{}, err
	}
	return sess, runner.Opened{
		Model:   info.Ref(),
		Root:    rt.env.Cwd,
		Release: func() { rt.env.Detach(sess) },
	}, nil
}

func webModelRef(backend llm.Backend) *llm.Ref {
	ref := llm.NewRef(backend)
	ref.OnLimits(func(limits llm.Limits) {
		if err := llm.SaveLimits(limits); err != nil {
			slog.Error("provider limits could not be saved", "provider", limits.Provider, "err", err)
		}
	})
	return ref
}

// runNotifier carries a run's changes to the connected pages.
//
// It exists because of an ordering the wiring cannot avoid: the registry is
// built before the daemon, since the daemon is served out of it. The server is
// filled in as soon as there is one, and a change published before that is
// dropped rather than queued - there is no page connected to hear it, because
// nothing is serving yet.
type runNotifier struct {
	mu  sync.Mutex
	srv *web.Server
}

func (n *runNotifier) to(srv *web.Server) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.srv = srv
}

func (n *runNotifier) publish(v runner.RunView) {
	n.mu.Lock()
	srv := n.srv
	n.mu.Unlock()
	if srv == nil {
		return
	}
	srv.Publish("run.updated", webRun(v))
}

// daemonNotifier wires backend-owned mutations to the control stream after the
// Server exists, just as runNotifier does for registry-owned run changes.
type daemonNotifier struct {
	mu  sync.Mutex
	srv *web.Server
}

func (n *daemonNotifier) to(srv *web.Server) {
	n.mu.Lock()
	n.srv = srv
	n.mu.Unlock()
}

func (n *daemonNotifier) publish(kind string, data any) {
	n.mu.Lock()
	srv := n.srv
	n.mu.Unlock()
	if srv != nil {
		srv.Publish(kind, data)
	}
}
