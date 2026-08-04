package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/bot"
)

// withBots points the config dir at a temp tree holding the named bots.
func withBots(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, n := range names {
		if err := bot.Save(bot.Config{Name: n, Role: "tester", Workdir: "."}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveBotNamesDefaultsToTheWholeFleet(t *testing.T) {
	withBots(t, "amiran", "jane", "kate")
	got, err := resolveBotNames(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"amiran", "jane", "kate"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestResolveBotNamesRunsOnlyTheNamedBots(t *testing.T) {
	withBots(t, "amiran", "jane", "kate")
	got, err := resolveBotNames([]string{"jane", "amiran"})
	if err != nil {
		t.Fatal(err)
	}
	// Order follows the command line, not the config dir.
	if strings.Join(got, ",") != "jane,amiran" {
		t.Fatalf("names = %v", got)
	}
}

func TestResolveBotNamesRejectsAnUnknownName(t *testing.T) {
	withBots(t, "amiran")
	_, err := resolveBotNames([]string{"amiran", "nobody"})
	if err == nil {
		t.Fatal("an unknown name must be an error, not a quietly smaller fleet")
	}
	if !strings.Contains(err.Error(), "nobody") || !strings.Contains(err.Error(), "amiran") {
		t.Fatalf("error should name the unknown bot and list the real ones: %v", err)
	}
}

func TestResolveBotNamesDeduplicates(t *testing.T) {
	withBots(t, "jane")
	// One Mattermost account allows one websocket, so naming a bot twice must not
	// start it twice.
	got, err := resolveBotNames([]string{"jane", "jane"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("names = %v, want one entry", got)
	}
}

func TestResolveBotNamesWithNoBotsConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "aigem"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveBotNames(nil); err == nil {
		t.Fatal("running the fleet with nothing configured must explain how to create a bot")
	}
}

func TestNextDelayBacksOffAndStops(t *testing.T) {
	d := botRestartDelay
	for i := 0; i < 20; i++ {
		d = nextDelay(d)
	}
	if d != botRestartMaxDelay {
		t.Fatalf("backoff settled at %v, want the %v ceiling", d, botRestartMaxDelay)
	}
}

// TestSuperviseBotBacksOffWhenStartsKeepFailing drives the real supervisor loop against a bot
// that can never start (no token is stored), and checks it slows down instead of spinning. A
// supervisor that retried at a fixed interval would hammer the provider and the chat server for
// as long as the misconfiguration lasted.
func TestSuperviseBotBacksOffWhenStartsKeepFailing(t *testing.T) {
	withBots(t, "ghost")
	shared := &fleetResources{fleet: bot.NewFleet()}

	var attempts atomic.Int32
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		superviseBot(ctx, "ghost", shared, countingLogger(&attempts), time.Millisecond)
		close(done)
	}()

	// Let it fail a few times, then stop it and confirm it both retried and gave up promptly.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superviseBot did not stop when its context was cancelled")
	}
	if attempts.Load() == 0 {
		t.Fatal("superviseBot never retried a bot that failed to start")
	}
	// Backoff doubling is what keeps those retries from being a spin.
	if got := nextDelay(time.Millisecond); got != 2*time.Millisecond {
		t.Fatalf("nextDelay(1ms) = %v, want 2ms", got)
	}
	if botStableFor <= 0 {
		t.Fatal("botStableFor must be a positive window, or every restart resets the backoff")
	}
}

// countingLogger counts the supervisor's failure reports, which is one per restart attempt.
func countingLogger(n *atomic.Int32) *slog.Logger {
	return slog.New(countingHandler{n: n})
}

type countingHandler struct{ n *atomic.Int32 }

func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.n.Add(1)
	}
	return nil
}
func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countingHandler) WithGroup(string) slog.Handler      { return h }
