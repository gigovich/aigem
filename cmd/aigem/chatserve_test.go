package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
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

	srv, err := startChatServer(ctx, chatServerOpts{addr: "127.0.0.1:0"}, slog.New(slog.DiscardHandler))
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

	var page chat.Page[chat.Message]
	if err := c.do(ctx, "GET", "/api/chat/threads/"+views[0].ID+"/messages", nil, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || !strings.Contains(page.Items[0].Body, "reach the daemon") {
		t.Fatalf("the thread holds %+v, want the opening message", page.Items)
	}
	if page.More {
		t.Fatal("a one-message thread reported more pages")
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

	srv, err := startChatServer(t.Context(),
		chatServerOpts{addr: "127.0.0.1:0", names: []string{"nosuchbot"}}, slog.New(slog.DiscardHandler))
	if err == nil {
		srv.Close()
		t.Fatal("registering an unknown bot was accepted")
	}
	// The store is closed on that path, so the record must not be left behind.
	if _, err := os.Stat(stateRecord(t)); err == nil {
		t.Fatal("a failed start left a state record pointing at nothing")
	}
}

// What a turn cost is accumulated by the store and returned by the API long
// before anything shows it to a person. Until the browser mode exists the
// transcript is where the operator reads a thread, so it is where the number
// has to appear.
func TestReadingAThreadShowsWhatItCost(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := t.Context()

	srv, err := startChatServer(ctx, chatServerOpts{addr: "127.0.0.1:0"}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	const amiran, demetre = "bot:amiran", "bot:demetre"
	for _, id := range []string{amiran, demetre} {
		if err := srv.store.PutActor(ctx, chat.Actor{ID: id, Name: id[4:], Role: "developer"}); err != nil {
			t.Fatal(err)
		}
	}
	th, err := srv.store.NewThread(ctx, "retries", chat.Operator, []string{amiran, demetre})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.Say(ctx, th.ID,
		chat.Draft{Author: chat.Operator, Body: "the logout is back"}); err != nil {
		t.Fatal(err)
	}
	// Two bots, two models, and a call the provider reported nothing for - a
	// thread total has to survive all three.
	spend := func(actor string, u chat.Usage, model string) {
		t.Helper()
		turn, err := srv.store.BeginTurn(ctx, th.ID, actor)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.AddUsage(ctx, actor, turn, u, model); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.EndTurn(ctx, actor, turn, ""); err != nil {
			t.Fatal(err)
		}
	}
	spend(amiran, chat.Usage{InputTokens: 1200, CachedTokens: 400, OutputTokens: 340,
		Calls: 2, Uncounted: 1}, "xai/grok-4.3")
	spend(demetre, chat.Usage{InputTokens: 300, OutputTokens: 60, Calls: 1}, "xai/grok-4.2")
	// A second turn on a model already listed: the footer names each one once.
	spend(amiran, chat.Usage{InputTokens: 40, OutputTokens: 10, Calls: 1}, "xai/grok-4.3")
	// A turn that failed before it reached the provider is not work anyone paid
	// for, so it must not pad the turn count or name a model that cost nothing.
	spend(amiran, chat.Usage{}, "xai/grok-4.9")

	c, err := dialChat()
	if err != nil {
		t.Fatal(err)
	}
	// Through `read`, not straight to the footer: the footer is only worth
	// anything if the command a person types actually prints it.
	out := captureStdout(t, func() {
		if err := c.read(ctx, []string{th.ID}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "the logout is back") {
		t.Fatalf("read printed no transcript: %q", out)
	}
	// The whole line, not substrings of it: a footer assembled from the wrong
	// fields still contains every piece one would think to look for.
	want := "thread total: 3 turns · 1.5k in (400 cached) · 410 out · 4 calls " +
		"(1 uncounted) · xai/grok-4.2, xai/grok-4.3"
	if !strings.Contains(out, want) {
		t.Fatalf("the transcript footer is\n%q\nwant it to contain\n%q", out, want)
	}

	// The total is the thread's, not the printed window's: a number that moved
	// with --limit would be a different answer every time the same thread was read.
	windowed := captureStdout(t, func() {
		if err := c.read(ctx, []string{"--limit", "1", th.ID}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(windowed, "4 calls") {
		t.Fatalf("a windowed read reported a different total: %q", windowed)
	}

	// --json is for scripts, which want the messages they asked for and nothing
	// else wrapped around them.
	raw := captureStdout(t, func() {
		if err := c.read(ctx, []string{"--json", th.ID}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(raw, "thread total") {
		t.Fatalf("--json output carries the footer: %q", raw)
	}

	// A thread nobody has worked in yet has no cost, and printing a row of
	// zeroes under it would be noise rather than information.
	quiet, err := srv.store.NewThread(ctx, "quiet", chat.Operator, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() { c.printSpend(ctx, quiet.ID) }); out != "" {
		t.Fatalf("a thread that cost nothing printed %q", out)
	}
}

// A developer bot runs up to 120 model rounds a turn, so a thread's totals reach
// millions of tokens - and one turn with one call is the commonest thread there
// is. Both are the cases the footer renders in real life and neither is what a
// fixture reaches for.
func TestTheSpendFooterReadsAtEveryScale(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{
		{0, "0"}, {999, "999"}, {1000, "1.0k"}, {1200, "1.2k"},
		{999_949, "999.9k"},
		// Past here "%.1fk" would print "1000.0k", which is not how anyone writes
		// a million.
		{999_950, "1.0M"}, {1_000_000, "1.0M"}, {4_210_000, "4.2M"},
	} {
		if got := humanTokens(c.n); got != c.want {
			t.Errorf("humanTokens(%d) = %q, want %q", c.n, got, c.want)
		}
	}
	for _, c := range []struct {
		n    int
		want string
	}{{0, "0 turns"}, {1, "1 turn"}, {2, "2 turns"}} {
		if got := countOf(c.n, "turn"); got != c.want {
			t.Errorf("countOf(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func stateRecord(t *testing.T) string {
	t.Helper()
	return os.Getenv("XDG_STATE_HOME") + "/aigem/chat.json"
}

// Push is wired by the daemon, not by the packages it wires together: nothing
// else proves that the keys are generated where the store lives, that the API
// is told about them, or that the notifier is running at all.
func TestTheDaemonArmsPush(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, err := startChatServer(t.Context(), chatServerOpts{addr: "127.0.0.1:0"},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	if srv.push == nil {
		t.Fatal("the daemon came up with no notifier, so nothing will ever be pushed")
	}
	raw, err := os.ReadFile(filepath.Join(state, "aigem", "chat", "vapid.json"))
	if err != nil {
		t.Fatalf("the keys are not beside the store: %v", err)
	}
	var file struct {
		Public string `json:"public"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}

	c, err := dialChat()
	if err != nil {
		t.Fatal(err)
	}
	var key chat.PushAvailability
	if err := c.do(t.Context(), "GET", "/api/chat/push", nil, &key); err != nil {
		t.Fatal(err)
	}
	if !key.Available || key.Key != file.Public {
		t.Fatalf("the API offers %+v, want the key in the file (%s)", key, file.Public)
	}
}
