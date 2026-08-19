package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/llm"
)

// usageSSE is a streamed response carrying a usage chunk, which is the only
// thing on the wire that says what a call cost.
const usageSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":80,\"prompt_tokens_details\":" +
	"{\"cached_tokens\":20},\"completion_tokens\":9}}\n\ndata: [DONE]\n\n"

// This drives the seam joining three otherwise independent halves: the client
// reporting a call's cost with its context, the turn hanging its sink on that
// context, and the store recording it. Each half has its own test and none of
// them touches this one, so without it the reporting could be disconnected from
// the billing and every test would still pass.
//
// It does not cover startBot's one line calling this - there is no test that
// builds a bot - so deleting that line still costs nothing.
func TestABotsCallsAreBilledToTheTurnThatMadeThem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, usageSSE)
	}))
	defer srv.Close()

	client := llm.NewRef(llm.NewClient(llm.ClientConfig{
		BaseURL: srv.URL, Info: llm.ModelInfo{Provider: "xai", ID: "grok-4.3"},
	}))
	billUsageToTurn(client)

	var billed []string
	spend := func(u llm.Usage, model string) {
		billed = append(billed, fmt.Sprintf("%s %d/%d/%d",
			model, u.InputTokens, u.CachedTokens, u.OutputTokens))
	}
	ask := func(ctx context.Context) {
		t.Helper()
		if _, err := client.Stream(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
			nil, 0, func(llm.StreamEvent) {}); err != nil {
			t.Fatal(err)
		}
	}

	ask(bot.WithUsage(t.Context(), spend))
	if want := []string{"xai/grok-4.3 80/20/9"}; !slices.Equal(billed, want) {
		t.Fatalf("billed %v, want %v", billed, want)
	}

	// A heartbeat or a scheduled job has no turn, and must neither panic nor be
	// charged to whatever ran last.
	ask(t.Context())
	if len(billed) != 1 {
		t.Fatalf("a call outside a turn was billed: %v", billed)
	}
}

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

func TestOpenBotModelUsesBindingRoleDefaultsAndOverrides(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tt := range []struct {
		name string
		cfg  bot.Config
		want string
	}{
		{name: "architect default", cfg: bot.Config{Role: "architect", Workdir: t.TempDir()}, want: bot.DefaultArchitectModel},
		{name: "developer default", cfg: bot.Config{Role: "developer", Workdir: t.TempDir()}, want: bot.DefaultBotModel},
		{name: "configured override", cfg: bot.Config{Role: "architect", Model: bot.DefaultBotModel, Workdir: t.TempDir()}, want: bot.DefaultBotModel},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, err := openBotModel("worker", tt.cfg, log)
			if err != nil {
				t.Fatal(err)
			}
			if got := client.Model().Ref(); got != tt.want {
				t.Fatalf("opened %q, want binding selection %q", got, tt.want)
			}
		})
	}
}

func TestOpenBotModelDoesNotFallbackWhenRoleDefaultIsUnavailable(t *testing.T) {
	isolatedBots(t, "worker") // installs an authenticated no-auth fallback model
	t.Setenv("OPENAI_API_KEY", "")
	c, err := bot.Load("worker")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := openBotModel("worker", c, log)
	if err == nil {
		t.Fatalf("opened fallback model %q, want binding default failure", client.Model().Ref())
	}
	for _, want := range []string{"worker", "developer", bot.DefaultBotModel, bot.ModelSourceRoleDefault, "aigem bot model worker <ref>"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("startup error %q does not contain %q", err, want)
		}
	}
}

func TestOpenBotModelConfiguredErrorNamesBindingSource(t *testing.T) {
	isolatedBots(t, "worker")
	c, err := bot.Load("worker")
	if err != nil {
		t.Fatal(err)
	}
	c.Model = "locked/big"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err = openBotModel("worker", c, log)
	if err == nil {
		t.Fatal("unusable configured model opened")
	}
	for _, want := range []string{"worker", "developer", "locked/big", bot.ModelSourceConfigured, "aigem bot model worker <ref>"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("startup error %q does not contain %q", err, want)
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
	// Naming a bot twice asks for one bot, not two, so it must not
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
