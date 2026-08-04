package bot

import (
	"context"
	"log/slog"
	"runtime/debug"
)

// AgentBuilder returns a fresh agent for one scheduled run. Each run gets its own: a job's prompt
// is self-contained and carries no conversation, so there is nothing to reuse between them.
type AgentBuilder func() (Runner, error)

// NewCronRunner returns the callback the scheduler fires jobs through. It exists as a named
// function rather than a closure in the command layer because it holds real policy - the busy
// accounting a scheduled run owes the gate it is itself subject to, and how a run's answer feeds
// the heartbeat's cadence - and that policy is worth testing.
//
// enter marks agent work as in flight and returns the func that marks it finished; pass
// (*Runtime).EnterTurn. Without it the gate would only see chat turns, and a long scheduled run
// could be joined by the next one.
func NewCronRunner(log *slog.Logger, build AgentBuilder, hb *Heartbeat, enter func() func(),
	turnCap *TurnLimiter) RunFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, job CronJob) {
		// A scheduled run is a turn on its own goroutine, so it needs the same panic containment
		// a chat turn gets: one bot's bad job must not take the other bots in this process down.
		defer func() {
			if p := recover(); p != nil {
				log.Error("cron job panicked", "job", job.ID, "panic", p, "stack", string(debug.Stack()))
			}
		}()
		if enter != nil {
			defer enter()()
		}
		// A scheduled run costs the provider exactly what a chat turn costs, so it queues for the
		// same fleet-wide slot. Without this the cap bounds only chat, and a fleet's heartbeats -
		// which fire on their own schedule in every bot - go straight past it.
		release, aerr := turnCap.Acquire(ctx)
		if aerr != nil {
			return // shutting down while queued
		}
		defer release()
		ag, err := build()
		if err != nil {
			log.Error("cron job skipped", "job", job.ID, "err", err)
			return
		}
		log.Info("cron job start", "job", job.ID)
		answer, err := ag.Run(ctx, job.Prompt, CronEvents(log, job.ID))
		if err != nil {
			log.Error("cron job failed", "job", job.ID, "err", err)
			// A failing run still counts as unproductive, so a bot whose provider keeps refusing
			// backs off instead of paying for the same failure at the fastest cadence all day.
			if hb != nil {
				hb.AfterCronRun(job.ID, "")
			}
			return
		}
		log.Info("cron job done", "job", job.ID, "chars", len(answer))
		if hb != nil {
			hb.AfterCronRun(job.ID, answer)
		}
	}
}
