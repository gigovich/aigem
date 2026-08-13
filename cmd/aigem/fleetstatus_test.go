package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gigovich/aigem/internal/bot"
	"github.com/gigovich/aigem/internal/chat"
)

func armedBot(t *testing.T, name string, jobs []bot.CronJob) *runningBot {
	t.Helper()
	sched, warns := bot.NewScheduler(jobs, nil)
	if len(warns) != 0 {
		t.Fatalf("cron warnings: %v", warns)
	}
	hb := bot.NewHeartbeat(name, sched)
	if err := hb.Arm(); err != nil {
		t.Fatalf("arming the heartbeat: %v", err)
	}
	return &runningBot{model: "xai/grok-4.3", hb: hb, sched: sched}
}

// The one column an operator would otherwise read journalctl for: a configured
// bot that did not come up. It has to be reported as not running rather than
// left out, which is why the run's names are held separately from the bots that
// actually started.
func TestLiveFleetReportsAConfiguredBotThatNeverStarted(t *testing.T) {
	f := newLiveFleet([]string{"amiran", "demetre"})
	f.started("amiran", armedBot(t, "amiran", nil))

	st := f.status()
	if len(st) != 2 {
		t.Fatalf("status covers %d bots, want both configured ones: %+v", len(st), st)
	}
	up, ok := st[chat.BotActor("amiran")]
	if !ok || !up.Running {
		t.Fatalf("amiran reports %+v, want it running", up)
	}
	if up.Model != "xai/grok-4.3" || up.Heartbeat != "30m" || up.Tier != 0 {
		t.Errorf("amiran reports %+v, want its model and a working heartbeat", up)
	}
	down, ok := st[chat.BotActor("demetre")]
	if !ok {
		t.Fatal("a bot that never started is missing from the roster entirely")
	}
	if down.Running || down.Model != "" || down.Heartbeat != "" {
		t.Errorf("a stopped bot reports %+v, want nothing but not-running", down)
	}
}

// The heartbeat is installed as a built-in job, so every running bot always has
// something scheduled. A blank column would mean the two lists disagree.
func TestLiveFleetNamesTheNextJob(t *testing.T) {
	f := newLiveFleet([]string{"amiran"})
	// Every minute, so this job is always sooner than the heartbeat and the
	// assertion is about which job was chosen rather than about there being one.
	f.started("amiran", armedBot(t, "amiran", []bot.CronJob{
		{ID: "soon", Expr: "* * * * *", Prompt: "p"},
	}))

	live := f.status()[chat.BotActor("amiran")]
	if live.NextJob != "soon" {
		t.Fatalf("the next job is %q, want the one that fires every minute", live.NextJob)
	}
	if live.NextRun == nil || live.NextRun.IsZero() {
		t.Fatal("the next run is missing or the zero time, which the screen would draw as year 1")
	}
	if !live.NextRun.After(time.Now().Add(-time.Minute)) {
		t.Errorf("the next run is %s, which is not ahead of now", live.NextRun)
	}
}

// A bot that stops goes back to "not running" rather than keeping the handles of
// the run that ended - the teardown that unregisters it from the fleet roster is
// the same one that calls this.
func TestLiveFleetForgetsAStoppedBot(t *testing.T) {
	f := newLiveFleet([]string{"amiran"})
	f.started("amiran", armedBot(t, "amiran", nil))
	f.stopped("amiran")

	if live := f.status()[chat.BotActor("amiran")]; live.Running {
		t.Errorf("a stopped bot still reports %+v", live)
	}
}

// writeBot puts a minimal bot config where bot.Load will find it. registerActors
// reads one per name, so a roster test cannot skip this.
func writeBot(t *testing.T, name, role string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "aigem", "bots", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf("name: %s\nrole: %s\nworkdir: %s\n", name, role, t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "bot.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fleetRoster(t *testing.T, srv *chatServer) map[string]chat.FleetMember {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+srv.srv.Addr().String()+"/api/chat/fleet", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+srv.srv.Token())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/chat/fleet answered %d", res.StatusCode)
	}
	var rows []chat.FleetMember
	if err := json.NewDecoder(res.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	out := map[string]chat.FleetMember{}
	for _, m := range rows {
		out[m.ID] = m
	}
	return out
}

// The one line that joins this feature together, and the thing every part of it
// passes its own tests without.
//
// `liveFleet` knows the heartbeat and the model, the API can serve them, and the
// screen can draw them - but if nothing hands the one to the other, every
// operational column reads "-" forever and no unit test anywhere notices. This
// is that wire, exercised through the HTTP route the browser actually calls.
func TestTheDaemonServesWhatOnlyItKnows(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeBot(t, "amiran", "developer")
	writeBot(t, "demetre", "tester")

	live := newLiveFleet([]string{"amiran", "demetre"})
	live.started("amiran", armedBot(t, "amiran", nil))
	srv, err := startChatServer(t.Context(), chatServerOpts{
		addr: "127.0.0.1:0", names: []string{"amiran", "demetre"}, live: live,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	roster := fleetRoster(t, srv)
	up := roster[chat.BotActor("amiran")]
	if up.Live == nil {
		t.Fatal("the daemon served no live state at all: the fleet status was never installed")
	}
	if up.Live.Model != "xai/grok-4.3" || up.Live.Heartbeat != "30m" || up.Live.NextJob == "" {
		t.Errorf("amiran's live state reached the API as %+v", up.Live)
	}
	if up.State != chat.FleetIdle {
		t.Errorf("a started bot with no open turn is %q, want %q", up.State, chat.FleetIdle)
	}
	// And the bot the daemon could not start is the state an operator would
	// otherwise read journalctl for.
	if down := roster[chat.BotActor("demetre")]; down.State != chat.FleetStopped {
		t.Errorf("an unstarted bot is %q, want %q", down.State, chat.FleetStopped)
	}
}

// registerActors used to mark every configured bot present before any of them
// had started, which is what made a bot that never came up draw a running dot.
// The roster is where that shows, so it is where it is pinned.
func TestABotIsNotPresentUntilItStarts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeBot(t, "amiran", "developer")

	srv, err := startChatServer(t.Context(), chatServerOpts{
		addr: "127.0.0.1:0", names: []string{"amiran"},
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	before := fleetRoster(t, srv)[chat.BotActor("amiran")]
	if before.Present || before.State != chat.FleetStopped {
		t.Fatalf("a registered but unstarted bot reads present=%v state=%q", before.Present, before.State)
	}

	// What startBot does when the bot is actually up.
	setPresent(t.Context(), srv.store, chat.BotActor("amiran"), true, slog.New(slog.DiscardHandler))

	after := fleetRoster(t, srv)[chat.BotActor("amiran")]
	if !after.Present || after.State != chat.FleetIdle {
		t.Errorf("a started bot reads present=%v state=%q", after.Present, after.State)
	}
}

// The flag every deployment document tells the operator to pass. The origin
// check is tested in internal/web; this is the wire from the command line to it.
func TestTheOriginFlagReachesTheDaemon(t *testing.T) {
	addr, origins, names, err := chatAddrFlag(
		[]string{"--addr", "0.0.0.0:0", "--origin", "https://aigem.example.ts.net", "amiran"})
	if err != nil {
		t.Fatal(err)
	}
	if addr != "0.0.0.0:0" || len(origins) != 1 || origins[0] != "https://aigem.example.ts.net" {
		t.Fatalf("parsed addr=%q origins=%v", addr, origins)
	}
	if len(names) != 1 || names[0] != "amiran" {
		t.Fatalf("the names after the flags came out as %v", names)
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Without it, a bind the network can reach is refused - which is only true
	// if the flag actually arrives at web.Config.
	if srv, err := startChatServer(t.Context(),
		chatServerOpts{addr: "127.0.0.1:0"}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("loopback without an origin should serve: %v", err)
	} else {
		srv.Close()
	}
	_, err = startChatServer(t.Context(),
		chatServerOpts{addr: "0.0.0.0:0"}, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("a wildcard bind with no origin was accepted")
	}
	srv, err := startChatServer(t.Context(), chatServerOpts{
		addr: "0.0.0.0:0", origins: origins,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("a wildcard bind with an origin was refused: %v", err)
	}
	srv.Close()
}

func TestAnOriginMustNotBeEmpty(t *testing.T) {
	var o originList
	if err := o.Set("  "); err == nil {
		t.Error("an empty --origin was accepted")
	}
	if err := o.Set(" https://x.test "); err != nil || len(o) != 1 || o[0] != "https://x.test" {
		t.Errorf("Set trimmed to %v (%v)", o, err)
	}
}
