package main

import (
	"testing"

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
	f.started("amiran", armedBot(t, "amiran", []bot.CronJob{
		{ID: "nightly", Expr: "0 4 * * *", Prompt: "p"},
	}))

	live := f.status()[chat.BotActor("amiran")]
	if live.NextJob == "" || live.NextRun == nil {
		t.Fatalf("a running bot reports no next job: %+v", live)
	}
	if live.NextRun.IsZero() {
		t.Error("the next run is the zero time, which the screen would draw as year 1")
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
