package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gigovich/aigem/internal/tools"
)

type scheduleTool struct{ sched *Scheduler }

// NewScheduleTool lets the bot manage its own cron jobs. Not confirm-gated: the bot owns its
// schedule.
func NewScheduleTool(s *Scheduler) tools.Tool { return &scheduleTool{sched: s} }

func (t *scheduleTool) Name() string       { return "schedule" }
func (t *scheduleTool) NeedsConfirm() bool { return false }

func (t *scheduleTool) Description() string {
	return "Manage your own scheduled jobs. Actions: set (create or replace a job - give an id, a " +
		"prompt, and EITHER expr (a 5-field cron expression \"minute hour day-of-month month " +
		"day-of-week\" for a recurring job) OR delay (e.g. \"10m\", \"2h\", \"90m\" for a one-shot " +
		"that runs once after that delay and then deletes itself)); remove (delete a job by id); " +
		"list (show your jobs). Each scheduled run starts a fresh agent with only your memory, so " +
		"write each prompt as a self-contained instruction - include where to report the result " +
		"(channel, and the thread root id if it should land back in a specific thread). Setting an " +
		"id that exists replaces it."
}

func (t *scheduleTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["set","remove","list"]},
			"id":{"type":"string","description":"job id (required for set/remove)"},
			"expr":{"type":"string","description":"5-field cron expression for a recurring job (set: expr or delay)"},
			"delay":{"type":"string","description":"Go duration like \"10m\"/\"2h\" for a one-shot job that runs once then self-deletes (set: expr or delay)"},
			"prompt":{"type":"string","description":"the self-contained task to run (required for set)"}
		},
		"required":["action"]
	}`)
}

func (t *scheduleTool) Run(_ context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		Expr   string `json:"expr"`
		Delay  string `json:"delay"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(rawArgs, &a); err != nil {
		return "", err
	}
	switch a.Action {
	case "set":
		if a.ID == "" || strings.TrimSpace(a.Prompt) == "" {
			return "", fmt.Errorf("set requires id, prompt, and either expr or delay")
		}
		if (a.Expr == "") == (a.Delay == "") {
			return "", fmt.Errorf("set requires exactly one of expr (recurring) or delay (one-shot)")
		}
		job := CronJob{ID: a.ID, Expr: a.Expr, Prompt: a.Prompt}
		when := a.Expr
		if a.Delay != "" {
			d, err := time.ParseDuration(a.Delay)
			if err != nil {
				return "", fmt.Errorf("delay: %w", err)
			}
			if d <= 0 {
				return "", fmt.Errorf("delay must be positive")
			}
			fireAt := time.Now().Add(d).UTC()
			job.Expr = ""
			job.At = fireAt.Format(time.RFC3339)
			when = "once at " + job.At
		}
		if err := t.sched.Set(job); err != nil {
			return "", err
		}
		return fmt.Sprintf("scheduled job %q (%s)", a.ID, when), nil
	case "remove":
		if a.ID == "" {
			return "", fmt.Errorf("remove requires id")
		}
		if err := t.sched.Remove(a.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("removed job %q", a.ID), nil
	case "list":
		jobs := t.sched.List()
		if len(jobs) == 0 {
			return "(no scheduled jobs)", nil
		}
		var b strings.Builder
		for _, j := range jobs {
			when := j.Expr
			if j.At != "" {
				when = "once at " + j.At
			}
			mark := ""
			if t.sched.IsBuiltin(j.ID) {
				mark = " [built-in]"
			}
			fmt.Fprintf(&b, "%s%s: %s - %s\n", j.ID, mark, when, j.Prompt)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	default:
		return "", fmt.Errorf("unknown action %q; use set, remove, or list", a.Action)
	}
}
