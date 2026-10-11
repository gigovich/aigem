package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/pathutil"
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/search"
	"github.com/gigovich/aigem/internal/store"
	"github.com/gigovich/aigem/internal/web"
)

const webUsage = `usage:
  aigem web                       serve the browser UI on a loopback port
  aigem web --root ~/work         offer projects from under ~/work rather than
                                  the directory the daemon was started in
  aigem web --addr 127.0.0.1:7777 serve on a fixed port
  aigem web --open                open the page in the default browser
  aigem web --sign-out            forget every browser session, then serve
  aigem web --addr 0.0.0.0:7777 --origin https://aigem.example.ts.net
                                  serve where the network can reach, under a
                                  name requests are checked against

The printed URL carries the token the browser signs in with. The page trades it
for a cookie and takes it back out of the address bar, but until it does it is a
secret on stdout - and with --open, in the process table of this machine.

Browser sign-ins outlive a restart, so restarting does not revoke one: it
rotates the token and leaves every cookie working. If the token got out, stop
the daemon and start it again with --sign-out, which forgets every session
first. Stopping it is not optional: a daemon still running holds the sessions in
memory, goes on honouring every cookie, and records its next change against the
file --sign-out removed.

The daemon binds loopback unless --origin says which public URL it is reached
at. An address the network can reach needs an origin check, and nothing in a
request can be trusted to supply the name to check against. A loopback bind with
` + "`tailscale serve`" + ` or another reverse proxy in front of it needs no flag at all;
--origin is for terminating that proxy yourself.

A binary built with a plain "go build" carries no UI and says so when a page is
requested. Build one with "make web && make build".`

// originList collects a repeatable --origin. A daemon reached under two names -
// a tailnet name and a LAN one - needs both, and one flag per name is how every
// other repeatable flag in this binary reads.
type originList []string

func (o *originList) String() string { return strings.Join(*o, ",") }

func (o *originList) Set(v string) error {
	*o = append(*o, v)
	return nil
}

func runWebCommand(args []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	// Silenced so a parse error is reported once, by the caller, rather than
	// twice - the flag package's own line and then main's "error: " prefix.
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "", "listen address (default: a loopback port chosen by the kernel)")
	open := fs.Bool("open", false, "open the page in the default browser once it is serving")
	signOut := fs.Bool("sign-out", false,
		"forget every browser session before serving, so each one signs in again.\n"+
			"Stop any running daemon first, or it will keep honouring them.\n"+
			"Refuses to serve at all if the sessions cannot be forgotten")
	var origins originList
	fs.Var(&origins, "origin", "public origin this daemon is reached at, scheme and all;\n"+
		"repeat for more than one. Required to bind an address the network can reach")
	rootFlag := fs.String("root", "", "the directory new projects are picked from (default: the current one)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(webUsage)
			fmt.Println("\nflags:")
			fs.SetOutput(os.Stdout)
			fs.PrintDefaults()
			return nil
		}
		return fmt.Errorf("%w\n\n%s", err, webUsage)
	}

	// Every run belongs to a project, and the project registry lives in the
	// state directory: without one there is nothing the daemon could do.
	stateDir, err := config.StateDir()
	if err != nil {
		return fmt.Errorf("the state directory could not be found: %w", err)
	}
	cookies := filepath.Join(stateDir, "web-cookies.json")
	root, err := webRoot(*rootFlag)
	if err != nil {
		return err
	}

	if *signOut {
		if err := web.ForgetSessions(cookies); err != nil {
			return fmt.Errorf("could not forget the browser sessions: %w", err)
		}
	}

	if err := web.CheckBind(*addr, origins); err != nil {
		return err
	}

	// The web tools, if the operator has configured a provider for them.
	searchCfg, err := search.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not load search config:", err)
	}

	// The daemon it publishes to does not exist yet, so the notifier is filled
	// in below rather than at construction. Nothing can be missed in between:
	// the registry is empty until a request arrives, and no request can arrive
	// until Serve.
	var announce notifier

	// The registry before the runs, so its deferred Close runs after theirs: a
	// session being saved needs the environment its SessionEnd hook runs in.
	projects, err := runner.NewProjects(runner.ProjectsConfig{
		Store: store.New[runner.ProjectTable](filepath.Join(stateDir, "projects.json")),
		LoadEnv: func(ctx context.Context, dir string) (*runner.Env, error) {
			env, _, err := runner.Load(ctx, runner.Options{
				Cwd: dir, Version: versionString(), Search: searchCfg,
				Notify: func(n runner.Notice) { fmt.Fprintln(os.Stderr, "warning:", dir+":", n.Text) },
			})
			return env, err
		},
		Notify: announce.publishProject,
	})
	if err != nil {
		return err
	}
	defer projects.Close()
	tickets := runner.NewTickets(runner.TicketsConfig{Dir: stateDir, Notify: announce.publishTicket})
	rt := &webRuntime{models: sharedModels(defaultModelRegistry()), projects: projects}
	runs, err := rt.newRuns(stateDir, announce.publishRun)
	if err != nil {
		return err
	}
	defer runs.Close()

	// The feed is a record of what happened, not an audit log, and a page shows
	// the recent end of it. Anything older than this is dropped, which is also
	// what stops a daemon that has been up for months holding every line it ever
	// wrote - and what gives a client that can drive mutations a ceiling.
	const activityRetention = 30 * 24 * time.Hour
	activity := store.NewLog[web.Activity](filepath.Join(stateDir, "activity.jsonl"))
	// Trimmed here and not while serving: compaction rewrites the file, and
	// a page holding a cursor into the part being dropped would find its
	// next page empty rather than wrong. Startup is the one moment no page
	// holds one at all.
	if n, err := activity.Compact(time.Now().Add(-activityRetention)); err != nil {
		slog.Warn("the activity feed could not be trimmed", "err", err)
	} else if n > 0 {
		slog.Info("trimmed the activity feed", "dropped", n)
	}
	backend := newWebBackend(webBackendConfig{
		version: versionString(), models: rt.models, runs: runs,
		root: root, projects: projects, tickets: tickets, activity: activity, notify: announce.publish,
	})
	backend.ticketRuns = runner.NewTicketRuns(runner.TicketRunsConfig{
		Runs: runs, Tickets: tickets, Projects: projects,
		Finished: backend.ticketFinished, Planned: backend.ticketPlanned,
	})
	backend.ticketRuns.Recover()
	// Deferred after runs.Close, so it runs first: deliveries stop before the sessions do.
	defer backend.ticketRuns.Close()
	dispatcher := runner.NewDispatcher(runner.DispatcherConfig{
		Projects: projects, Tickets: tickets, TicketRuns: backend.ticketRuns,
		Started: backend.ticketStarted,
	})
	announce.wakes(dispatcher.Wake)
	// Deferred after ticketRuns.Close, so it runs before it: no start races their shutdown.
	defer dispatcher.Close()
	srv, err := web.New(web.Config{
		Addr:       *addr,
		Origins:    origins,
		Assets:     web.Assets(),
		CookieFile: cookies,
		Backend:    backend,
	})
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()
	announce.to(srv)

	// The one place the token is meant to be published: this terminal.
	url := srv.SignInURL()
	fmt.Println(url)
	// With --origin the printed link is the public one, which says nothing about
	// where the daemon actually bound - and with a kernel-chosen port there
	// would otherwise be no way to find out.
	if bound := "http://" + srv.Addr().String() + "/"; !strings.HasPrefix(url, bound) {
		fmt.Fprintln(os.Stderr, "listening on", srv.Addr())
	}
	if !web.HasAssets() {
		fmt.Fprintln(os.Stderr,
			"note: this binary carries no browser UI; build one with `make web && make build`")
	}
	if *open {
		openBrowser(url)
	}

	// Serve in the background so a signal can close the listener: Serve only
	// returns once the server is closed, and there is nothing else to interrupt
	// it from.
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-done:
		return err
	case <-sig:
		// Hand the signal back to the runtime so a second Ctrl-C during the wait
		// below kills the process rather than being swallowed.
		signal.Stop(sig)
		fmt.Fprintln(os.Stderr, "\nstopping")
		// The daemon's connections first, then the conversations behind them,
		// then the environment they ran in: a session being saved must not be
		// racing a client that is still submitting, and its SessionEnd hook
		// needs the environment to still be there.
		if err := srv.Close(); err != nil {
			return err
		}
		dispatcher.Close()
		backend.ticketRuns.Close()
		runs.Close()
		// Serve's error is the one worth reporting, so give it a moment to
		// surface rather than exiting on the signal alone.
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			return nil
		}
	}
}

// openBrowser is best-effort: failing to open a window is not a reason to
// refuse to serve, and the URL is already on stdout for the person to click.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not open a browser: %v\n", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}

// webRoot is the directory new projects are picked from, canonical so that a
// containment check against it compares like with like.
func webRoot(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	root, err := pathutil.Canonical(dir)
	if err != nil {
		return "", fmt.Errorf("--root: %w", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", fmt.Errorf("--root: %s is not a directory", root)
	}
	return root, nil
}
