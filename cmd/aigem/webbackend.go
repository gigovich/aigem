package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/gigovich/aigem/internal/auth"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/store"
	"github.com/gigovich/aigem/internal/uisession"
	"github.com/gigovich/aigem/internal/web"
)

// webBackend is the wiring internal/web is deliberately unable to do itself:
// the web package declares what it needs and never imports internal/runner or
// internal/llm, so the binary is the one place that knows how a model
// reference is resolved and what this build is called.
type webBackend struct {
	version string
	// models is the configured providers, read once: the file behind it is the
	// operator's, not the daemon's, and a change to it is a restart either way.
	models *llm.Registry
	// runs is the daemon's conversations. Everything under /api/runs is this
	// registry, translated.
	runs *runner.Runs
	env  *runner.Env

	activity   *store.Log[web.Activity]
	activityMu sync.Mutex
	notify     func(string, any)

	flowMu       sync.Mutex
	flows        map[string]*auth.Flow
	flowOrder    []string
	flowSeq      uint64
	flowStarting map[string]int
	flowWG       sync.WaitGroup
	flowCtx      context.Context
	flowCancel   context.CancelFunc
	closed       bool
	closeOnce    sync.Once
	beginFlow    func(context.Context, string) (*auth.Flow, error)

	skillMu sync.Mutex
	closeMu sync.Mutex
}

type webBackendOptions struct {
	env      *runner.Env
	activity *store.Log[web.Activity]
	notify   func(string, any)
}

func newWebBackend(version string, models *llm.Registry, runs *runner.Runs, options ...webBackendOptions) *webBackend {
	if models == nil {
		models = defaultModelRegistry()
	}
	var opts webBackendOptions
	if len(options) > 0 {
		opts = options[0]
	}
	flowCtx, flowCancel := context.WithCancel(context.Background())
	return &webBackend{
		version: version, models: models, runs: runs, env: opts.env, activity: opts.activity,
		notify: opts.notify, flows: map[string]*auth.Flow{}, flowStarting: map[string]int{},
		beginFlow: auth.Begin, flowCtx: flowCtx, flowCancel: flowCancel,
	}
}

// Meta reads the saved preference and the credential store on every call
// rather than caching the answer. Both change while the daemon runs - signing a
// provider in and picking a model are things the UI itself does - and a cached
// default model would go on naming a provider the operator has just signed out
// of. It is a couple of small file reads, on a route a page asks for once.
func (b *webBackend) Meta(_ context.Context) (web.Meta, error) {
	return web.Meta{
		Version:      b.version,
		DefaultModel: preferredModelRef(b.models),
	}, nil
}

const (
	// maxArtifactChange is the largest change this route carries the content
	// of - both versions of the file together. A quarter of a megabyte is a
	// very large source file and a small fraction of what a generated one can
	// be.
	maxArtifactChange = 256 << 10
	// maxArtifactBody is the budget for the contents in one response. A run
	// that touched a hundred files is a real run; a hundred files' worth of
	// content in one JSON document is not a page anyone can render.
	maxArtifactBody = 4 << 20
)

// The run half of the backend: the registry's answers, translated into the
// shapes internal/web marshals and the errors it maps to status codes.

func (b *webBackend) Runs(context.Context) ([]web.Run, error) {
	views := b.runs.List()
	out := make([]web.Run, 0, len(views))
	for _, v := range views {
		out = append(out, webRun(v))
	}
	return out, nil
}

func (b *webBackend) OpenRun(ctx context.Context, req web.NewRun) (web.Run, error) {
	v, err := b.runs.Create(ctx, runner.RunRequest{
		Mode:  runner.Mode(req.Mode),
		Title: req.Title,
		Model: req.Model,
	})
	if err != nil {
		return web.Run{}, webRunError(err)
	}
	out := webRun(v)
	b.recordActivity(web.Activity{Kind: "run.created", Text: "Run created", RunRef: out.ID})
	return out, nil
}

func (b *webBackend) Run(_ context.Context, id string) (web.Run, error) {
	v, err := b.runs.Get(id)
	if err != nil {
		return web.Run{}, webRunError(err)
	}
	return webRun(v), nil
}

func (b *webBackend) CloseRun(_ context.Context, id string) error {
	// Serialize the read-before-close so two tabs closing together append one
	// activity record rather than both observing the run live.
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	before, err := b.runs.Get(id)
	if err != nil {
		return webRunError(err)
	}
	if err := b.runs.CloseRun(id); err != nil {
		return webRunError(err)
	}
	if before.Live {
		b.recordActivity(web.Activity{Kind: "run.closed", Text: "Run closed", RunRef: id})
	}
	return nil
}

func (b *webBackend) RunEvents(_ context.Context, id string, since uint64, limit int) (
	[]web.RunEvent, error,
) {
	evs, err := b.runs.Events(id, since, limit)
	if err != nil {
		return nil, webRunError(err)
	}
	out := make([]web.RunEvent, 0, len(evs))
	for _, ev := range evs {
		enc, err := json.Marshal(ev)
		if err != nil {
			// The whole page would be a hole with one event missing from the
			// middle of it, so this is the daemon's failure and not a partial
			// answer dressed up as a timeline.
			return nil, fmt.Errorf("encoding event %d of run %s: %w", ev.Seq, id, err)
		}
		out = append(out, enc)
	}
	return out, nil
}

func (b *webBackend) RunBlob(_ context.Context, id string, seq uint64) (string, error) {
	body, err := b.runs.Blob(id, seq)
	if err != nil {
		return "", webRunError(err)
	}
	return body, nil
}

func (b *webBackend) WatchRun(_ context.Context, id string, c web.RunClient, since uint64) (
	web.RunStream, error,
) {
	events, detach, err := b.runs.Subscribe(id,
		uisession.Client{Kind: c.Kind, Label: c.Label}, since)
	if err != nil {
		return nil, webRunError(err)
	}
	s := &runStream{out: make(chan web.RunEvent), detach: detach, done: make(chan struct{})}
	go s.encode(events)
	return s, nil
}

func (b *webBackend) RunArtifacts(_ context.Context, id string) ([]web.Artifact, error) {
	arts, err := b.runs.Artifacts(id)
	if err != nil {
		return nil, webRunError(err)
	}
	// Sorted, because the map is not: a page that redraws the list on every
	// poll must not shuffle it.
	paths := make([]string, 0, len(arts))
	for p := range arts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]web.Artifact, 0, len(paths))
	budget := maxArtifactBody
	for _, p := range paths {
		c := arts[p]
		a := web.Artifact{
			Path: c.Path, Created: c.Created,
			OldBytes: len(c.Old), NewBytes: len(c.New),
		}
		// A file the agent changed can be any size, and both versions of it are
		// already in memory; putting them in a response copies them again, and
		// the encoder buffers the whole array before a byte goes out. The list
		// of what changed is always complete - it is only the content that is
		// rationed.
		switch size := len(c.Old) + len(c.New); {
		case size > maxArtifactChange, size > budget:
			a.Truncated = true
		default:
			budget -= size
			a.Old, a.New = c.Old, c.New
		}
		out = append(out, a)
	}
	return out, nil
}

func (b *webBackend) ApplyRunOp(_ context.Context, id string, op web.RunOp) error {
	images := make([]llm.Image, 0, len(op.Images))
	for _, im := range op.Images {
		images = append(images, llm.Image{MediaType: im.MediaType, Data: im.Data})
	}
	return webRunError(b.runs.Apply(id, runner.RunOp{
		Op:       op.Op,
		Text:     op.Text,
		Images:   images,
		ID:       op.ID,
		Decision: uisession.Decision(op.Decision),
		By:       op.Label,
		Name:     op.Name,
		Args:     op.Args,
		On:       op.On,
		Ref:      op.Ref,
		Persist:  op.Persist,
	}))
}

// runStream turns the session's events into encoded frames on their own
// goroutine, so that a slow encode is not something the session's fan-out waits
// for - and so that internal/web never sees the event type.
type runStream struct {
	out    chan web.RunEvent
	detach func()
	done   chan struct{}
	once   sync.Once
}

func (s *runStream) Events() <-chan web.RunEvent { return s.out }

// Close detaches and unblocks the encoder. It is idempotent, as the interface
// promises: the handler defers it and may also close early.
func (s *runStream) Close() {
	s.once.Do(func() {
		close(s.done)
		s.detach()
	})
}

// encode forwards until the session's stream ends or the client goes away. It
// owns the channel it writes to, so it is the one that closes it - which is how
// the reader learns the run has ended.
func (s *runStream) encode(events <-chan uisession.Event) {
	defer close(s.out)
	for ev := range events {
		enc, err := json.Marshal(ev)
		if err != nil {
			// One event that cannot be encoded is not a reason to end the
			// conversation, and the client sees a gap in the sequence, which is
			// the same thing it already handles after a drop.
			slog.Error("a run event could not be encoded", "seq", ev.Seq, "err", err)
			continue
		}
		select {
		case s.out <- enc:
		case <-s.done:
			return
		}
	}
}

func webRun(v runner.RunView) web.Run {
	return web.Run{
		ID: v.ID, SessionID: v.SessionID, Mode: string(v.Mode), Title: v.Title,
		Model: v.Model, Root: v.Root, Status: string(v.Status),
		Created: v.Created, Updated: v.Updated,
		Live: v.Live, Running: v.Running, Waiting: v.Waiting, Step: v.Step, Seq: v.Seq,
	}
}

// webRunError classifies what the registry reports.
//
// The default is a Refusal, which is the opposite of the usual instinct and is
// the right way round here: this daemon serves one signed-in person on their own
// machine, and almost everything that can go wrong with a run is something they
// can act on - a provider that is not signed in, an approval somebody else
// answered first, a command that does not exist. Hiding those behind "the
// daemon could not carry that out" would leave the operator with a button that
// stops working and no way to find out why.
//
// The exception is the daemon's own state. A registry that is shutting down is
// not the client's mistake, and telling a page it made one would be a lie it
// would then show to the person.
func webRunError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runner.ErrNoRun):
		return web.ErrNoRun
	case errors.Is(err, runner.ErrNoBlob):
		return web.ErrNoBlob
	case errors.Is(err, runner.ErrRunClosed), errors.Is(err, uisession.ErrClosed):
		// The second is the same answer arriving from further in: the table
		// handed out a session that was closed underneath it between the lookup
		// and the call. A client keys on the status code, and "session closed"
		// as a 400 is a refusal it cannot act on.
		return web.ErrRunClosed
	case errors.Is(err, uisession.ErrTruncated):
		return web.ErrHistoryGone
	case errors.Is(err, runner.ErrTooManyRuns):
		// Wrapped rather than replaced: the sentinel decides the status code and
		// the text says how many are open, which is what tells the person to
		// close one rather than to wait.
		return fmt.Errorf("%w: %w", web.ErrBusy, err)
	case errors.Is(err, runner.ErrRunsClosed):
		return err
	default:
		return web.Refuse(err)
	}
}
