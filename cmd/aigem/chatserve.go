package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/web"
)

// chatServer is the conversation store, its fan-out and the daemon serving
// them, all owned by the fleet process.
//
// It lives with the fleet rather than beside it because they are the same
// thing: the bots write to this store, and the browser and the CLI read it.
// Two processes would mean two SQLite writers and a protocol between them, for
// no gain.
type chatServer struct {
	store *chat.Store
	hub   *chat.Hub
	srv   *web.Server
}

// pruneAfter is how long a thread's agent timeline is kept. What was said is
// the record and is never pruned; how an agent got there is large - five bots
// on heartbeats write a timeline all day - and stops earning its disk within
// days.
const pruneAfter = 30 * 24 * time.Hour

// pruneEvery is how often the sweep runs. It is not on the hour: a fleet that
// restarts at a fixed time would otherwise never reach it.
const pruneEvery = 6 * time.Hour

// chatServerOpts is what the fleet's daemon is started with.
type chatServerOpts struct {
	// addr is the listen address; empty means loopback on a port the OS picks.
	addr string
	// origins are the public URLs the daemon is reached at. Serving on anything
	// but loopback without one is refused; see internal/web.
	origins []string
	// names are the bots of this run, whose identities are registered before
	// any of them starts.
	names []string
	// live may be nil, for a daemon that serves the store without running any
	// bots - the roster then reports only what the store can count.
	live *liveFleet
}

// startChatServer opens the store, registers the fleet's identities, and serves
// the API.
func startChatServer(ctx context.Context, o chatServerOpts, log *slog.Logger) (*chatServer, error) {
	dir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	store, err := chat.Open(ctx, filepath.Join(dir, "chat"))
	if err != nil {
		return nil, err
	}
	hub := chat.NewHub()
	_ = store.AddPublisher("hub", hub.Publish)

	if err := registerActors(ctx, store, o.names); err != nil {
		_ = store.Close()
		return nil, err
	}
	// A turn with no process behind it is not running, and an inbox that says
	// otherwise is worse than one that says nothing.
	if n, err := store.CloseStaleTurns(ctx); err != nil {
		_ = store.Close()
		return nil, err
	} else if n > 0 {
		log.Info("closed turns left open by a previous run", "turns", n)
	}

	api := chat.NewAPI(store, hub)
	if o.live != nil {
		api.SetFleetStatus(o.live.status)
	}
	srv, err := web.New(web.Config{
		Addr:    o.addr,
		Origins: o.origins,
		Assets:  webAssets(),
		Mount:   api.Mount,
	})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := chat.SaveState(chat.State{
		PID: os.Getpid(), Addr: srv.Addr().String(), Token: srv.Token(),
	}); err != nil {
		_ = srv.Close()
		_ = store.Close()
		return nil, err
	}

	go func() {
		if err := srv.Serve(); err != nil {
			log.Error("chat server stopped", "err", err)
		}
	}()
	go prune(ctx, store, log)

	fmt.Println("chat UI: " + srv.URL())
	return &chatServer{store: store, hub: hub, srv: srv}, nil
}

// registerActors records the operator and every bot in the run, so a renamed
// role or a newly added bot is reflected without a migration. Presence is
// cleared first: a process that was killed had no chance to clear its own flag,
// and the only honest reading of the column after a crash is "unknown".
//
// A bot is registered as not present. This runs before a single bot has been
// started, and marking them present here made the flag mean "configured" - so a
// bot whose model could not be opened showed a running dot beside its name in
// the inbox, the composer and every participant list. startBot sets it, and its
// teardown clears it.
func registerActors(ctx context.Context, store *chat.Store, names []string) error {
	if err := store.ClearPresence(ctx); err != nil {
		return err
	}
	if err := store.PutActor(ctx, chat.Actor{ID: chat.Operator, Name: "operator"}); err != nil {
		return err
	}
	for _, name := range names {
		cfg, err := bot.Load(name)
		if err != nil {
			return err
		}
		if err := store.PutActor(ctx, chat.Actor{
			ID: chat.BotActor(name), Name: name, Role: cfg.Role,
		}); err != nil {
			return err
		}
	}
	return nil
}

// prune trims old timeline events until the process ends.
func prune(ctx context.Context, store *chat.Store, log *slog.Logger) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			events, blobs, diffs, err := store.Prune(ctx, time.Now().Add(-pruneAfter))
			if err != nil {
				log.Warn("pruning the timeline failed", "err", err)
				continue
			}
			removed, err := store.SweepBlobs(ctx)
			if err != nil {
				log.Warn("sweeping attachments failed", "err", err)
			}
			// diffs is its own key, and named apart from "files": that one counts
			// swept attachment uploads. A sweep that dropped a million stored
			// diffs and no events used to log nothing at all.
			if events > 0 || diffs > 0 || removed > 0 {
				log.Info("pruned", "events", events, "blobs", blobs,
					"diffs", diffs, "files", removed)
			}
		}
	}
}

// Close stops serving and releases the store. The state record goes with it, so
// the next `aigem chat` does not try to reach a daemon that has gone.
func (c *chatServer) Close() {
	if c == nil {
		return
	}
	// The hub first: http.Server.Close does not touch a hijacked connection, so
	// an attached socket would otherwise outlive the daemon and then answer
	// every op with "database is closed".
	c.hub.Close()
	_ = c.srv.Close()
	_ = c.store.Close()
	_ = chat.ClearState()
}

// webAssets returns the built UI if this binary carries one. A plain `go build`
// has none on purpose, and the API is still the whole product without it.
func webAssets() http.Handler { return web.Assets() }

// chatAddrFlag parses the fleet's serving address out of the command's
// arguments, so `aigem bot start` can be pointed somewhere other than a random
// loopback port without every other flag having to move to the flag package.
func chatAddrFlag(args []string) (addr string, origins []string, rest []string, err error) {
	fs := flag.NewFlagSet("bot start", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&addr, "addr", "", "address to serve the chat UI on (default: a loopback port)")
	fs.Var((*originList)(&origins), "origin",
		"public URL this daemon is reached at, e.g. https://aigem.example.ts.net "+
			"(required for a non-loopback --addr; repeat for more than one)")
	if err := fs.Parse(args); err != nil {
		return "", nil, nil, err
	}
	return addr, origins, fs.Args(), nil
}

// originList collects a repeated --origin.
//
// Repeated rather than comma-separated: an origin is a URL, a URL may contain a
// comma, and a flag that silently splits one in half would produce two entries
// that match nothing - which is the failure this whole flag exists to prevent
// being mysterious.
type originList []string

func (o *originList) String() string { return strings.Join(*o, ",") }

func (o *originList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("an origin cannot be empty")
	}
	*o = append(*o, v)
	return nil
}
