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
	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/search"
	"github.com/gigovich/aigem/internal/store"
	"github.com/gigovich/aigem/internal/web"
)

const webUsage = `usage:
  aigem web                       serve the browser UI on a loopback port
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
	// The same three the terminal takes, because the warnings a withheld
	// capability produces name them by name: without these, the only way to
	// follow the daemon's own advice is to start the TUI in this directory
	// first, which is not advice anybody should have to work out.
	trustHooks := fs.Bool("trust-project-hooks", false,
		"approve this project's local hooks as they are now")
	trustMCP := fs.Bool("trust-project-mcp", false,
		"approve this project's local MCP servers as they are now")
	trustSkills := fs.Bool("trust-project-skills", false,
		"approve this project's local skills as they are now")
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

	// A failure to find the state directory is reported and not fatal: the
	// operator locked out of the UI would be locked out by the one thing the UI
	// is for, and /healthz and the page still answer.
	//
	// It is not a state the daemon works in, though. Opening a model reads the
	// credential store, which is under the same directory, so every attempt to
	// start a conversation will fail with whatever went wrong here. What is
	// saved is the ability to see that, and to fix it.
	cookies, stateDir := "", ""
	if dir, err := config.StateDir(); err != nil {
		fmt.Fprintf(os.Stderr, "note: browser sign-ins and the list of runs will not "+
			"survive a restart: %v\n", err)
	} else {
		stateDir = dir
		cookies = filepath.Join(dir, "web-cookies.json")
	}

	if *signOut {
		// Refused rather than skipped: the operator asked for a revocation, and
		// serving on as if it had happened is the answer they cannot check.
		if cookies == "" {
			return errors.New("--sign-out: there is no browser sessions file to forget, " +
				"because the state directory could not be found")
		}
		if err := web.ForgetSessions(cookies); err != nil {
			return fmt.Errorf("could not forget the browser sessions: %w", err)
		}
	}

	// Before the environment: loading it runs the person's SessionStart hook and
	// starts their MCP servers, and a daemon that is going to refuse what the
	// operator typed must not do either on its way to saying so.
	if err := web.CheckBind(*addr, origins); err != nil {
		return err
	}

	// The web tools, if the operator has configured a provider for them.
	searchCfg, err := search.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not load search config:", err)
	}

	// The environment is loaded once and shared by every conversation the
	// daemon opens: the skills, the subagents, the hooks configuration and the
	// MCP servers belong to the project, and one set of stdio servers per
	// browser tab is not a thing anyone wants.
	//
	// The directory is the one the daemon was started in: an empty Cwd resolves
	// to it, and there is no flag for another because the project a browser
	// session works in is a phase-2 choice.
	//
	// A daemon has nobody to ask about a withheld capability once it is
	// running, so the --trust-project-* decisions are made here or not at all:
	// without one of those flags a project's local hooks, MCP servers and
	// skills stay withheld. Skills are the exception, because the browser has a
	// screen for them - POST /api/skills/trust is the same decision, asked at
	// the moment it matters.
	env, _, err := runner.Load(context.Background(), runner.Options{
		Version:            versionString(),
		Search:             searchCfg,
		TrustProjectHooks:  *trustHooks,
		TrustProjectMCP:    *trustMCP,
		TrustProjectSkills: *trustSkills,
		// Raised as they happen rather than collected: Load dials the MCP
		// servers and runs the SessionStart hook, and a terminal that says
		// nothing until those finish reads as a hang.
		Notify: func(n runner.Notice) {
			fmt.Fprintln(os.Stderr, "warning:", n.Text)
		},
	})
	if err != nil {
		return err
	}
	// Closed after the runs below, so that a session's SessionEnd hook still
	// has the environment it runs in.
	defer env.Close()
	if env.SystemMessage != "" {
		fmt.Fprintln(os.Stderr, env.SystemMessage)
	}

	// The daemon it publishes to does not exist yet, so the notifier is filled
	// in below rather than at construction. Nothing can be missed in between:
	// the registry is empty until a request arrives, and no request can arrive
	// until Serve.
	var announce notifier

	// The registry before the runs, so its deferred Close runs after theirs: a
	// session being saved needs the environment its SessionEnd hook runs in.
	// Without a state directory there is no registry, and the page is told so
	// through the feature map.
	var projects *runner.Projects
	if stateDir != "" {
		projects, err = runner.NewProjects(runner.ProjectsConfig{
			Store: store.New[[]runner.Project](filepath.Join(stateDir, "projects.json")),
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
	}
	rt := &webRuntime{env: env, models: defaultModelRegistry(), projects: projects}
	runs, err := rt.newRuns(stateDir, announce.publishRun)
	if err != nil {
		return err
	}
	defer runs.Close()

	var activity *store.Log[web.Activity]
	// The feed is a record of what happened, not an audit log, and a page shows
	// the recent end of it. Anything older than this is dropped, which is also
	// what stops a daemon that has been up for months holding every line it ever
	// wrote - and what gives a client that can drive mutations a ceiling.
	const activityRetention = 30 * 24 * time.Hour
	if stateDir != "" {
		activity = store.NewLog[web.Activity](filepath.Join(stateDir, "activity.jsonl"))
		// Trimmed here and not while serving: compaction rewrites the file, and
		// a page holding a cursor into the part being dropped would find its
		// next page empty rather than wrong. Startup is the one moment no page
		// holds one at all.
		if n, err := activity.Compact(time.Now().Add(-activityRetention)); err != nil {
			slog.Warn("the activity feed could not be trimmed", "err", err)
		} else if n > 0 {
			slog.Info("trimmed the activity feed", "dropped", n)
		}
	}
	backend := newWebBackend(webBackendConfig{
		version: versionString(), models: rt.models, runs: runs,
		env: env, projects: projects, activity: activity, notify: announce.publish,
	})
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
