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
	env      *runner.Env
	models   *llm.Registry
	projects *runner.Projects
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
func (rt *webRuntime) openRun(ctx context.Context, req runner.RunRequest) (
	*runner.Session, runner.Opened, error,
) {
	env := rt.env
	if req.ProjectID != "" {
		if rt.projects == nil {
			return nil, runner.Opened{}, errors.New("this daemon serves no projects")
		}
		var err error
		if env, err = rt.projects.Env(ctx, req.ProjectID); err != nil {
			return nil, runner.Opened{}, err
		}
	}
	if env == nil {
		return nil, runner.Opened{}, errors.New("no environment is loaded")
	}
	reg, err := env.NewTools()
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
		sp, injected := env.SystemPrompt()
		reg.MarkInContext(injected)
		return sp
	}
	title := req.Title
	if title == "" {
		title = env.SessionTitle
	}
	backendRef := webModelRef(backend)
	sess := runner.NewSession(runner.Spec{
		Mode:    req.Mode,
		Tools:   reg,
		Backend: backendRef,
		Models:  rt.models,
		Agents:  env.Agents,
		Skills:  env.Skills,
		Hooks:   env.Hooks,
		Project: env.Project,
		System:  buildSystem(),
		Title:   title,
		// A conversation started in a browser runs the person's own hooks, and
		// a hook that is told nothing about where it is running would resolve
		// paths against wherever the daemon happens to have been started.
		Cwd:           env.Cwd,
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
	sess.HandleCommands(env.Skills, env.MCP)
	// Approving the project's skills has to reach every live conversation, not
	// just the one that asked.
	if err := env.Attach(sess); err != nil {
		sess.Local.Close()
		return nil, runner.Opened{}, err
	}
	return sess, runner.Opened{
		Model:   info.Ref(),
		Root:    env.Cwd,
		Release: func() { env.Detach(sess) },
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

// notifier carries a mutation to the connected pages.
//
// It exists because of an ordering the wiring cannot avoid: the run registry
// and the backend are both built before the daemon, since the daemon is served
// out of them. The server is filled in as soon as there is one, and anything
// published before that is dropped rather than queued - there is no page
// connected to hear it, because nothing is serving yet.
type notifier struct {
	mu  sync.Mutex
	srv *web.Server
}

func (n *notifier) to(srv *web.Server) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.srv = srv
}

func (n *notifier) publish(kind string, data any) {
	n.mu.Lock()
	srv := n.srv
	n.mu.Unlock()
	if srv == nil {
		return
	}
	srv.Publish(kind, data)
}

// publishRun is the registry's own callback shape: it takes the runner's view
// and names the one kind a run change is ever announced as.
func (n *notifier) publishRun(v runner.RunView) { n.publish("run.updated", webRun(v)) }

// publishProject is the projects registry's own callback shape, the same way.
func (n *notifier) publishProject(v runner.ProjectView) { n.publish("project.updated", webProject(v)) }
