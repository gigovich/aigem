package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/chat"
)

// The fleet's daemon and the operator's CLI have to actually meet: the server
// has to write the state record, and `aigem chat` has to find it there. Nothing
// in the package tests proves that, because both halves pass their own suite
// while the file between them is never written.
func TestTheCLIReachesTheDaemonTheFleetStarts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := t.Context()

	srv, err := startChatServer(ctx, "127.0.0.1:0", nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, err := dialChat()
	if err != nil {
		t.Fatalf("the CLI could not find the daemon the fleet just started: %v", err)
	}

	// Open a thread, say something in it, and read it back - the operator's
	// whole path, through the state file and the HTTP API.
	if err := c.do(ctx, "POST", "/api/chat/threads", map[string]any{
		"title": "smoke", "text": "does the CLI reach the daemon",
	}, nil); err != nil {
		t.Fatal(err)
	}
	var views []chat.ThreadView
	if err := c.do(ctx, "GET", "/api/chat/threads", nil, &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Title != "smoke" {
		t.Fatalf("the daemon lists %+v, want the thread just opened", views)
	}

	var msgs []chat.Message
	if err := c.do(ctx, "GET", "/api/chat/threads/"+views[0].ID+"/messages", nil, &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "reach the daemon") {
		t.Fatalf("the thread holds %+v, want the opening message", msgs)
	}

	// And the record goes when the daemon does, so the next `aigem chat` does
	// not try to reach something that has stopped.
	srv.Close()
	if _, running, err := chat.LoadState(); err != nil || running {
		t.Fatalf("the state record survived shutdown: running=%v err=%v", running, err)
	}
	if _, err := dialChat(); err == nil {
		t.Fatal("the CLI still believes a daemon is running")
	}
}

// A bot that cannot be started must not take the daemon with it before the
// operator has any way to see why.
func TestTheDaemonComesUpBeforeAnyBot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, err := startChatServer(t.Context(), "127.0.0.1:0", []string{"nosuchbot"}, slog.New(slog.DiscardHandler))
	if err == nil {
		srv.Close()
		t.Fatal("registering an unknown bot was accepted")
	}
	// The store is closed on that path, so the record must not be left behind.
	if _, err := os.Stat(stateRecord(t)); err == nil {
		t.Fatal("a failed start left a state record pointing at nothing")
	}
}

func stateRecord(t *testing.T) string {
	t.Helper()
	return os.Getenv("XDG_STATE_HOME") + "/aigem/chat.json"
}
