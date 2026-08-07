package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/trace"
)

// Run is one scored execution of one scenario.
type Run struct {
	Scenario string `json:"scenario"`
	Attempt  int    `json:"attempt"`
	// TaskCalls is how many task calls the model issued, Delegations how many
	// subagents actually started. They differ when a call is rejected before it
	// runs - an unknown agent_type, an empty prompt, a hook or a denied
	// confirmation - and the gap matters: the model still made the choice to
	// delegate, so it counts against precision even though nothing ran.
	TaskCalls   int `json:"task_calls"`
	Delegations int `json:"delegations"`
	// MaxBatch is how many task calls the largest single assistant message
	// carried; MaxParallel how many subagents that message actually started.
	// Only the second is evidence of concurrent work - a batch whose calls were
	// all rejected shows the intent and nothing else - so MaxParallel is what
	// min_parallel is checked against and MaxBatch is reported beside it.
	MaxBatch    int      `json:"max_batch"`
	MaxParallel int      `json:"max_parallel"`
	Agents      []string `json:"agents,omitempty"`
	ToolRounds  int      `json:"tool_rounds"`
	PeakTokens  int      `json:"peak_tokens"`
	// Answer is the run's final text, kept so the outcome assertions can be
	// checked against it.
	Answer string `json:"-"`
	// Vague holds delegated prompts that lean on context the subagent cannot
	// see. It is a keyword heuristic, reported but never failed on.
	Vague []string `json:"vague_prompts,omitempty"`
	// WrongAgent records that no delegation reached an agent the scenario accepts,
	// so the summary does not have to re-read the failure text. OtherAgents are
	// the additional agents a passing run also used - reported, because a second
	// step is usually legitimate and occasionally is not.
	WrongAgent  bool     `json:"wrong_agent,omitempty"`
	OtherAgents []string `json:"other_agents,omitempty"`
	// OutcomeFailed marks a run that did not do the work, whatever it did about
	// delegating. Such a run earns no credit in any rate: "did not delegate" is
	// only a good answer when the task got done anyway.
	OutcomeFailed bool `json:"outcome_failed,omitempty"`
	// Truncated marks a run stopped by a runaway budget. It ends without an
	// error and with a plausible answer, but the behavior after the cut is
	// missing, so it is not evidence either way.
	Truncated bool `json:"truncated,omitempty"`
	// Harness marks a failure of the runner rather than of the model.
	Harness bool `json:"harness_failure,omitempty"`
	// Failures is empty for a passing run.
	Failures []string `json:"failures,omitempty"`
	Err      string   `json:"error,omitempty"`
}

func (r Run) Passed() bool { return len(r.Failures) == 0 && r.Err == "" }

// Measured reports whether the run is evidence about the model's behavior at
// all. A crashed or budget-truncated run is not.
func (r Run) Measured() bool { return r.Err == "" && !r.Truncated && !r.Harness }

// contextReferences are phrases a self-contained prompt has no business using.
// The subagent cannot see the parent conversation, so any of these means the
// prompt is pointing at something the subagent will never receive. Kept short
// and specific: this is a smell detector, and a noisy one is worse than none.
var contextReferences = []string{
	"as mentioned", "as discussed", "as noted", "as described above",
	"mentioned above", "described above", "discussed above", "listed above",
	"as i said", "as we discussed", "we discussed", "the user asked", "the user wants",
	"this conversation", "earlier in this", "from the previous", "as before",
	"like i said", "same as the other", "continue where",
}

// ScoreRun turns a recorded trace into a verdict against the scenario.
func ScoreRun(s Scenario, attempt int, events []trace.Event) Run {
	r := Run{Scenario: s.Name, Attempt: attempt}
	// taskIDs are the ids of the task calls seen so far; a batch always precedes
	// the starts it caused, so it is filled before it is read. startedIn counts
	// subagents per model round, which is how many ran together rather than in
	// total. tasksIn backs the fallback below.
	taskIDs := map[string]bool{}
	tasksIn, startedIn := map[int]int{}, map[int]int{}
	batchHasIDs := false

	for _, e := range events {
		if e.Round > r.ToolRounds {
			r.ToolRounds = e.Round
		}
		switch e.Kind {
		case trace.KindToolBatch:
			n := 0
			for _, c := range e.Calls {
				if c.Tool != agent.TaskToolName {
					continue
				}
				n++
				if c.ID != "" {
					taskIDs[c.ID] = true
					batchHasIDs = true
				}
			}
			r.TaskCalls += n
			tasksIn[e.Round] += n
			if n > r.MaxBatch {
				r.MaxBatch = n
			}
		case trace.KindAgentStart:
			// A forked skill announces itself through the same channel as a
			// subagent, so the id is what tells them apart: only a run whose id
			// belongs to a task call is a delegation. A batch of task calls and a
			// forked skill in the SAME response is exactly the case a round-level
			// check gets wrong.
			if !startedByTask(e, taskIDs, tasksIn, batchHasIDs) {
				continue
			}
			r.Delegations++
			startedIn[e.Round]++
			if startedIn[e.Round] > r.MaxParallel {
				r.MaxParallel = startedIn[e.Round]
			}
			r.Agents = append(r.Agents, e.Agent)
			if hit := contextReference(e.Text); hit != "" {
				r.Vague = append(r.Vague, fmt.Sprintf("%s: %q", e.Agent, hit))
			}
		case trace.KindUsage:
			if e.Tokens > r.PeakTokens {
				r.PeakTokens = e.Tokens
			}
		case trace.KindBudget:
			r.Truncated = true
		case trace.KindRunEnd:
			r.Answer = e.Text
			if e.Error != "" {
				r.Err = e.Error
			}
		}
	}
	r.Failures = checkExpectations(s, &r)
	return r
}

func checkExpectations(s Scenario, r *Run) []string {
	var fail []string
	switch s.delegateMode() {
	case delegateRequired:
		if r.Delegations == 0 {
			fail = append(fail, "did not delegate")
		}
	case delegateForbidden:
		// Counted on task calls, not on subagents started: choosing to delegate is
		// the behavior being measured, and a call that errored out was still that
		// choice.
		if r.TaskCalls > 0 {
			fail = append(fail, fmt.Sprintf("delegated %d time(s) on a task that does not need it", r.TaskCalls))
		}
	}
	// Not reported when delegating was forbidden: there the task call itself is
	// already the failure, and a second line for the same mistake reads as two.
	if r.TaskCalls > r.Delegations && s.delegateMode() != delegateForbidden {
		fail = append(fail, fmt.Sprintf("%d task call(s) never started a subagent (rejected or denied)",
			r.TaskCalls-r.Delegations))
	}
	// "At least one", not "only these". A run legitimately uses several agents for
	// different steps - scout to find the file, then reviewer to read it; or an
	// edit made directly and a reviewer delegated to check it. Requiring every
	// agent to be listed failed all of those, and every such failure this suite
	// ever produced turned out to be one of them, with the work itself done.
	if len(s.Expect.Agents) > 0 && r.Delegations > 0 {
		allowed := map[string]bool{}
		for _, a := range s.Expect.Agents {
			allowed[a] = true
		}
		hit := false
		for _, got := range uniqueSorted(r.Agents) {
			if allowed[got] {
				hit = true
			} else {
				r.OtherAgents = append(r.OtherAgents, got)
			}
		}
		if !hit {
			r.WrongAgent = true
			fail = append(fail, fmt.Sprintf("delegated to %s but never to %s",
				strings.Join(uniqueSorted(r.Agents), "/"), strings.Join(s.Expect.Agents, "/")))
		}
	}
	if s.Expect.MinParallel > 0 && r.MaxParallel < s.Expect.MinParallel {
		fail = append(fail, fmt.Sprintf("at most %d subagent(s) ran together (%d task call(s) batched), want %d",
			r.MaxParallel, r.MaxBatch, s.Expect.MinParallel))
	}
	// Only bites once the run has chosen to delegate more than once: delegating
	// once, or not at all, is a different question and this is not it.
	if s.Expect.Batched && r.Delegations >= 2 && r.MaxParallel < 2 {
		fail = append(fail, fmt.Sprintf("%d subagents were run one at a time; independent ones belong in one response",
			r.Delegations))
	}
	if s.Expect.MaxTasks > 0 && r.TaskCalls > s.Expect.MaxTasks {
		fail = append(fail, fmt.Sprintf("issued %d task calls, cap is %d", r.TaskCalls, s.Expect.MaxTasks))
	}
	return fail
}

// startedByTask reports whether a nested run came from a task call rather than
// from a forked skill. Matching is by id; the round fallback exists only for a
// trace recorded before batches carried ids, where returning false for every
// start would read as a model that never delegates.
func startedByTask(e trace.Event, taskIDs map[string]bool, tasksIn map[int]int, batchHasIDs bool) bool {
	if batchHasIDs && e.ID != "" {
		return taskIDs[e.ID]
	}
	return tasksIn[e.Round] > 0
}

func contextReference(prompt string) string {
	low := strings.ToLower(prompt)
	for _, p := range contextReferences {
		if strings.Contains(low, p) {
			return p
		}
	}
	return ""
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Summary aggregates runs into the numbers worth tracking between prompt
// revisions.
type Summary struct {
	Runs int `json:"runs"`
	Pass int `json:"pass"`
	// Recall is passes over scenarios that require delegation; Precision is
	// passes over scenarios that forbid it (i.e. resisting over-delegation).
	RecallPass, RecallTotal       int `json:"-"`
	PrecisionPass, PrecisionTotal int `json:"-"`
	AgentPass, AgentTotal         int `json:"-"`
	ParallelPass, ParallelTotal   int `json:"-"`
	VaguePrompts, TotalPrompts    int `json:"-"`
	OutcomeFailures               int `json:"outcome_failures"`
	Errors                        int `json:"errors"`
	TruncatedRuns                 int `json:"truncated"`
}

// Summarize folds per-run verdicts into suite-level rates.
func Summarize(scenarios []Scenario, runs []Run) Summary {
	byName := map[string]Scenario{}
	for _, sc := range scenarios {
		byName[sc.Name] = sc
	}
	var s Summary
	for _, r := range runs {
		sc, ok := byName[r.Scenario]
		if !ok {
			continue
		}
		s.Runs++
		if r.Passed() {
			s.Pass++
		}
		// A run that crashed, was cut off by a budget, or broke the harness
		// measured nothing. Leaving it in the rate denominators would let a broken
		// backend report perfect precision, since a run that never started also
		// never delegated.
		if !r.Measured() {
			if r.Err != "" {
				s.Errors++
			}
			if r.Truncated {
				s.TruncatedRuns++
			}
			continue
		}
		// A run that skipped the work says nothing useful about delegation either.
		// Whichever way it went, it went that way while not doing the task, so it
		// is counted apart from every rate rather than folded into one.
		if r.OutcomeFailed {
			s.OutcomeFailures++
			continue
		}
		switch sc.delegateMode() {
		case delegateRequired:
			s.RecallTotal++
			if r.Delegations > 0 {
				s.RecallPass++
			}
		case delegateForbidden:
			s.PrecisionTotal++
			if r.TaskCalls == 0 {
				s.PrecisionPass++
			}
		}
		if len(sc.Expect.Agents) > 0 && r.Delegations > 0 {
			s.AgentTotal++
			if !r.WrongAgent {
				s.AgentPass++
			}
		}
		if sc.Expect.MinParallel > 0 {
			s.ParallelTotal++
			if r.MaxParallel >= sc.Expect.MinParallel {
				s.ParallelPass++
			}
		}
		// A batched scenario only contributes once it actually delegated twice;
		// counting the runs that chose not to would measure the decision, not the
		// batching.
		if sc.Expect.Batched && r.Delegations >= 2 {
			s.ParallelTotal++
			if r.MaxParallel >= 2 {
				s.ParallelPass++
			}
		}
		s.TotalPrompts += r.Delegations
		s.VaguePrompts += len(r.Vague)
	}
	return s
}

func ratio(pass, total int) string {
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", pass, total, 100*float64(pass)/float64(total))
}
