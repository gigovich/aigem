package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/gigovich/aigem/internal/web"
)

const webUsage = `usage:
  aigem web                       serve the browser UI on a loopback port
  aigem web --addr 127.0.0.1:7777 serve on a fixed port
  aigem web --open                open the page in the default browser

The daemon binds loopback only. To reach it from another device, put a reverse
proxy in front of it - ` + "`tailscale serve`" + ` is the supported shape - rather than
binding an address the network can reach.

A binary built with a plain "go build" carries no UI and says so when a page is
requested. Build one with "make web && make build".`

func runWebCommand(args []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	// Silenced so a parse error is reported once, by the caller, rather than
	// twice - the flag package's own line and then main's "error: " prefix.
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "", "listen address (default: a loopback port chosen by the kernel)")
	open := fs.Bool("open", false, "open the page in the default browser once it is serving")
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

	srv, err := web.New(web.Config{Addr: *addr, Assets: web.Assets()})
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()

	url := srv.URL()
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
