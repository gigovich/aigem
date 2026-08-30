package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gigovich/aigem/internal/config"
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
rotates the token and leaves every cookie working. If the token got out, use
--sign-out, which forgets every session before serving.

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
		"forget every browser session before serving, so each one signs in again")
	var origins originList
	fs.Var(&origins, "origin", "public origin this daemon is reached at, scheme and all;\n"+
		"repeat for more than one. Required to bind an address the network can reach")
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

	// A failure to find the state directory costs the browser sessions their
	// persistence, not the daemon its start: the operator locked out of the UI
	// would be locked out by the one thing the UI is for.
	cookies := ""
	if dir, err := config.StateDir(); err != nil {
		fmt.Fprintf(os.Stderr, "note: browser sign-ins will not survive a restart: %v\n", err)
	} else {
		cookies = filepath.Join(dir, "web-cookies.json")
	}

	if *signOut {
		if err := web.ForgetSessions(cookies); err != nil {
			return fmt.Errorf("could not forget the browser sessions: %w", err)
		}
	}

	srv, err := web.New(web.Config{
		Addr:       *addr,
		Origins:    origins,
		Assets:     web.Assets(),
		CookieFile: cookies,
	})
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()

	// The one place the token is meant to be published: this terminal.
	url := srv.SignInURL()
	fmt.Println(url)
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
		if err := srv.Close(); err != nil {
			return err
		}
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
