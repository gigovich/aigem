package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/trace"
)

// Real traces stamp every event with the model round of the batch that issued
// it and with the id of the call it came from; the scorer needs the round to
// tell concurrent delegations from consecutive ones, and the id to tell a
// subagent from a forked skill. The helpers carry both for the same reason, and
// synthesize ids the way the agent does so a test cannot pass on a shape the
// recorder never produces.
func batch(round int, names ...string) trace.Event {
	calls := make([]trace.Call, len(names))
	for i, n := range names {
		calls[i] = trace.Call{ID: callID(round, i), Tool: n}
	}
	return trace.Event{Kind: trace.KindToolBatch, Round: round, Calls: calls}
}

// started attributes the run to the first call of its round, which is what a
// single-task batch produces.
func started(round int, agent, prompt string) trace.Event {
	return startedBy(callID(round, 0), round, agent, prompt)
}

func startedBy(id string, round int, agent, prompt string) trace.Event {
	return trace.Event{Kind: trace.KindAgentStart, Round: round, ID: id, Agent: agent, Text: prompt}
}

func callID(round, i int) string { return fmt.Sprintf("c%d-%d", round, i) }

func TestForbiddenDelegationFails(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateForbidden}}

	direct := ScoreRun(s, 1, []trace.Event{batch(1, "read_file")})
	if !direct.Passed() {
		t.Fatalf("answering directly must pass: %v", direct.Failures)
	}
	delegated := ScoreRun(s, 1, []trace.Event{batch(1, "task"), started(1, "scout", "look at flush.go")})
	if delegated.Passed() {
		t.Fatal("delegating a trivial task must fail")
	}
}

func TestRequiredDelegationFailsWhenAbsent(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired}}
	r := ScoreRun(s, 1, []trace.Event{batch(1, "grep"), batch(1, "read_file")})
	if r.Passed() {
		t.Fatal("a scenario that requires delegation must fail when none happened")
	}
	if !strings.Contains(r.Failures[0], "did not delegate") {
		t.Fatalf("unhelpful failure text: %q", r.Failures[0])
	}
}

// Three task calls spread over three responses run one after another. Only the
// batch width tells them apart from three concurrent ones, and that is the
// whole point of the check.
func TestSequentialDelegationsAreNotParallel(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired, MinParallel: 3}}

	sequential := ScoreRun(s, 1, []trace.Event{
		batch(1, "task"), started(1, "scout", "a"),
		batch(2, "task"), started(2, "scout", "b"),
		batch(3, "task"), started(3, "scout", "c"),
	})
	if sequential.Passed() {
		t.Fatal("three sequential delegations must not count as parallel")
	}
	if sequential.Delegations != 3 || sequential.MaxBatch != 1 {
		t.Fatalf("delegations=%d maxbatch=%d, want 3 and 1", sequential.Delegations, sequential.MaxBatch)
	}

	concurrent := ScoreRun(s, 1, []trace.Event{
		batch(1, "task", "task", "task"),
		started(1, "scout", "a"), started(1, "scout", "b"), started(1, "scout", "c"),
	})
	if !concurrent.Passed() {
		t.Fatalf("one response with three task calls must pass: %v", concurrent.Failures)
	}
}

// Non-task calls sharing the batch must not inflate the parallelism count.
func TestMixedBatchCountsOnlyTaskCalls(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired, MinParallel: 2}}
	r := ScoreRun(s, 1, []trace.Event{batch(1, "task", "read_file", "grep"), started(1, "scout", "a")})
	if r.Passed() {
		t.Fatalf("one task call among three tools is not two-way parallelism: %v", r)
	}
	if r.MaxBatch != 1 {
		t.Fatalf("MaxBatch = %d, want 1", r.MaxBatch)
	}
}

// The check is "at least one", not "only these". A run that finds the file with
// scout and then reviews it, or edits directly and delegates a review, is doing
// the job in steps - and every wrong-agent failure this suite produced before
// this change was one of those, with the work itself correctly done.
func TestAgentCheckWantsOneMatchNotAllOfThem(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateOptional, Agents: []string{"reviewer"}}}

	twoSteps := ScoreRun(s, 1, []trace.Event{
		batch(1, "task"), started(1, "scout", "find flush.go"),
		batch(2, "task"), started(2, "reviewer", "review flush.go"),
	})
	if !twoSteps.Passed() {
		t.Fatalf("scout then reviewer is a plan, not a wrong choice: %v", twoSteps.Failures)
	}
	if len(twoSteps.OtherAgents) != 1 || twoSteps.OtherAgents[0] != "scout" {
		t.Errorf("the extra agent should still be reported, got %v", twoSteps.OtherAgents)
	}

	never := ScoreRun(s, 2, []trace.Event{batch(1, "task"), started(1, "scout", "find flush.go")})
	if never.Passed() || !never.WrongAgent {
		t.Fatal("a run that never reached the wanted agent must fail")
	}

	// Not delegating at all is a question for `delegate`, not for this check.
	none := ScoreRun(s, 3, []trace.Event{batch(1, "read_file")})
	if !none.Passed() {
		t.Fatalf("not delegating under delegate:optional must pass: %v", none.Failures)
	}
}

func TestMaxTasksCatchesSwarming(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired, MaxTasks: 2}}
	r := ScoreRun(s, 1, []trace.Event{
		batch(1, "task", "task", "task"),
		started(1, "scout", "a"), started(1, "scout", "b"), started(1, "scout", "c"),
	})
	if r.Passed() {
		t.Fatal("exceeding the delegation cap must fail")
	}
}

// A task call that is rejected before the subagent starts (unknown agent_type,
// empty prompt, a hook or a denied confirmation) leaves no agent_start. Counting
// only agent_start would score that as "did not delegate" - and a scenario that
// forbids delegating would pass on a run that tried to delegate and failed.
func TestFailedTaskCallStillCountsAsDelegating(t *testing.T) {
	forbidden := Scenario{Name: "x", Expect: Expect{Delegate: delegateForbidden}}
	r := ScoreRun(forbidden, 1, []trace.Event{
		batch(1, "task"),
		{Kind: trace.KindToolEnd, Tool: "task", Error: `unknown agent_type "researcher"`},
	})
	if r.Passed() {
		t.Fatal("a task call that errored is still a decision to delegate")
	}
	if r.TaskCalls != 1 || r.Delegations != 0 {
		t.Fatalf("task_calls=%d delegations=%d, want 1 and 0", r.TaskCalls, r.Delegations)
	}

	s := Summarize([]Scenario{forbidden}, []Run{r})
	if s.PrecisionPass != 0 {
		t.Fatal("precision must count the attempt, or the summary contradicts the verdict")
	}
}

// The same gap in the other direction: a batch of two task calls that both die
// proves intent to parallelize but nothing actually ran concurrently.
func TestParallelMetricRequiresSubagentsToStart(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired, MinParallel: 2}}
	r := ScoreRun(s, 1, []trace.Event{batch(1, "task", "task")})
	sum := Summarize([]Scenario{s}, []Run{r})
	if sum.ParallelPass != 0 {
		t.Fatal("two failed task calls must not count as parallel compliance")
	}
	if r.Passed() {
		t.Fatal("the per-run verdict must fail too")
	}
}

func TestSummarizeCountsAgentsAndVaguePrompts(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired, Agents: []string{"scout"}}}
	good := ScoreRun(s, 1, []trace.Event{batch(1, "task"), started(1, "scout", "Read flush.go")})
	bad := ScoreRun(s, 2, []trace.Event{batch(1, "task"), started(1, "reviewer", "Review it as we discussed")})

	sum := Summarize([]Scenario{s}, []Run{good, bad})
	if sum.AgentPass != 1 || sum.AgentTotal != 2 {
		t.Fatalf("agent accuracy = %d/%d, want 1/2", sum.AgentPass, sum.AgentTotal)
	}
	if sum.TotalPrompts != 2 || sum.VaguePrompts != 1 {
		t.Fatalf("prompts = %d, vague = %d, want 2 and 1", sum.TotalPrompts, sum.VaguePrompts)
	}
}

// "as well" is ordinary English and used to trip the heuristic.
func TestHeuristicDoesNotFireOnOrdinaryPhrasing(t *testing.T) {
	s := Scenario{Name: "x"}
	r := ScoreRun(s, 1, []trace.Event{
		batch(1, "task"), started(1, "code-writer", "Add the flag, and run the linter as well as the tests"),
	})
	if len(r.Vague) != 0 {
		t.Fatalf("a clean prompt was flagged: %v", r.Vague)
	}
}

// A forked skill announces itself through the same event as a subagent. When
// one response batches a task call AND a forked skill, both starts land in the
// same round, so only the call id can tell them apart: counting by round would
// inflate delegations, add the skill to Agents, and satisfy min_parallel with a
// single real subagent.
func TestForkedSkillInTheSameBatchIsNotADelegation(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired, MinParallel: 2, Agents: []string{"scout"}}}
	r := ScoreRun(s, 1, []trace.Event{
		{Kind: trace.KindToolBatch, Round: 1, Calls: []trace.Call{
			{ID: "t1", Tool: "task"},
			{ID: "s1", Tool: "skill"},
		}},
		startedBy("t1", 1, "scout", "Read services/alpha"),
		startedBy("s1", 1, "release-checklist", "run the checklist"),
	})
	if r.Delegations != 1 {
		t.Errorf("delegations = %d, want 1 - the forked skill is not one", r.Delegations)
	}
	if r.MaxParallel != 1 {
		t.Errorf("max_parallel = %d, want 1", r.MaxParallel)
	}
	if len(r.Agents) != 1 || r.Agents[0] != "scout" {
		t.Errorf("agents = %v, want just the subagent", r.Agents)
	}
	if r.WrongAgent {
		t.Error("the skill name must not be judged as a wrong agent choice")
	}
	if r.Passed() {
		t.Error("one subagent does not satisfy min_parallel 2")
	}
}

// The inverse: a rejected task call plus a forked skill in the same round must
// not look like a delegation that succeeded.
func TestForkedSkillDoesNotCoverARejectedTaskCall(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired}}
	r := ScoreRun(s, 1, []trace.Event{
		{Kind: trace.KindToolBatch, Round: 1, Calls: []trace.Call{
			{ID: "t1", Tool: "task"},
			{ID: "s1", Tool: "skill"},
		}},
		{Kind: trace.KindToolEnd, Round: 1, Tool: "task", Error: `unknown agent_type "researcher"`},
		startedBy("s1", 1, "release-checklist", "run the checklist"),
	})
	if r.Delegations != 0 || r.TaskCalls != 1 {
		t.Fatalf("task_calls=%d delegations=%d, want 1 and 0", r.TaskCalls, r.Delegations)
	}
	if r.Passed() {
		t.Fatal("a task call that never started a subagent must fail")
	}
}

// Traces recorded before batches carried ids fall back to the round, which is
// the old behavior: worse, but better than scoring every run as zero.
func TestIDlessTraceFallsBackToTheRound(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired}}
	r := ScoreRun(s, 1, []trace.Event{
		{Kind: trace.KindToolBatch, Round: 1, Calls: []trace.Call{{Tool: "task"}}},
		{Kind: trace.KindAgentStart, Round: 1, Agent: "scout"},
	})
	if r.Delegations != 1 {
		t.Fatalf("delegations = %d, want 1 from the round fallback", r.Delegations)
	}
}

// batched asks how a run delegates, not whether. Declining to delegate, or
// delegating once, answers a different question and must not fail here.
func TestBatchedScoresOnlyRunsThatDelegatedTwice(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateOptional, Batched: true}}

	solo := ScoreRun(s, 1, []trace.Event{batch(1, "read_file"), batch(2, "grep")})
	once := ScoreRun(s, 2, []trace.Event{batch(1, "task"), started(1, "scout", "a")})
	for _, r := range []Run{solo, once} {
		if !r.Passed() {
			t.Fatalf("attempt %d must pass - batching says nothing about it: %v", r.Attempt, r.Failures)
		}
	}

	together := ScoreRun(s, 3, []trace.Event{
		batch(1, "task", "task"),
		startedBy(callID(1, 0), 1, "scout", "a"), startedBy(callID(1, 1), 1, "scout", "b"),
	})
	if !together.Passed() {
		t.Fatalf("two subagents in one response must pass: %v", together.Failures)
	}

	sequential := ScoreRun(s, 4, []trace.Event{
		batch(1, "task"), started(1, "scout", "a"),
		batch(2, "task"), started(2, "scout", "b"),
	})
	if sequential.Passed() {
		t.Fatal("two subagents run one after another must fail a batched scenario")
	}

	// Only the runs that delegated twice enter the rate.
	sum := Summarize([]Scenario{s}, []Run{solo, once, together, sequential})
	if sum.ParallelPass != 1 || sum.ParallelTotal != 2 {
		t.Fatalf("parallel rate = %d/%d, want 1/2", sum.ParallelPass, sum.ParallelTotal)
	}
}

func TestRunErrorIsRecorded(t *testing.T) {
	s := Scenario{Name: "x"}
	r := ScoreRun(s, 1, []trace.Event{{Kind: trace.KindRunEnd, Error: "context deadline exceeded"}})
	if r.Passed() || r.Err == "" {
		t.Fatal("a failed run must not be scored as a pass")
	}
}

func TestVaguePromptsAreReportedNotFailed(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired}}
	r := ScoreRun(s, 1, []trace.Event{
		batch(1, "task"), started(1, "scout", "Check the file as mentioned above and report back"),
	})
	if !r.Passed() {
		t.Fatalf("the heuristic must not fail a run: %v", r.Failures)
	}
	if len(r.Vague) != 1 {
		t.Fatalf("expected the context reference to be flagged, got %v", r.Vague)
	}
	clean := ScoreRun(s, 1, []trace.Event{
		batch(1, "task"), started(1, "scout", "Read flush.go and list its retry limits"),
	})
	if len(clean.Vague) != 0 {
		t.Fatalf("a self-contained prompt was flagged: %v", clean.Vague)
	}
}

func TestPeakTokensAndRounds(t *testing.T) {
	r := ScoreRun(Scenario{Name: "x"}, 1, []trace.Event{
		{Kind: trace.KindUsage, Tokens: 900, Round: 1},
		{Kind: trace.KindUsage, Tokens: 400, Round: 4},
	})
	if r.PeakTokens != 900 {
		t.Fatalf("PeakTokens = %d, want the maximum 900", r.PeakTokens)
	}
	if r.ToolRounds != 4 {
		t.Fatalf("ToolRounds = %d, want 4", r.ToolRounds)
	}
}

func TestSummarizeSeparatesRecallFromPrecision(t *testing.T) {
	need := Scenario{Name: "need", Expect: Expect{Delegate: delegateRequired}}
	avoid := Scenario{Name: "avoid", Expect: Expect{Delegate: delegateForbidden}}

	runs := []Run{
		ScoreRun(need, 1, []trace.Event{batch(1, "task"), started(1, "scout", "a")}),
		ScoreRun(need, 2, []trace.Event{batch(1, "grep")}),
		ScoreRun(avoid, 1, []trace.Event{batch(1, "read_file")}),
		ScoreRun(avoid, 2, []trace.Event{batch(1, "read_file")}),
	}
	s := Summarize([]Scenario{need, avoid}, runs)
	if s.RecallPass != 1 || s.RecallTotal != 2 {
		t.Fatalf("recall = %d/%d, want 1/2", s.RecallPass, s.RecallTotal)
	}
	if s.PrecisionPass != 2 || s.PrecisionTotal != 2 {
		t.Fatalf("precision = %d/%d, want 2/2", s.PrecisionPass, s.PrecisionTotal)
	}
	if s.Pass != 3 || s.Runs != 4 {
		t.Fatalf("pass = %d/%d, want 3/4", s.Pass, s.Runs)
	}
}

func TestShippedScenariosAreValid(t *testing.T) {
	scenarios, err := LoadScenarios(filepath.Join("..", "scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scenarios {
		if s.Why == "" {
			t.Errorf("scenario %q has no `why`: an expectation nobody can justify later is noise", s.Name)
		}
		dir := filepath.Join("..", "testdata", s.Fixture)
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("scenario %q points at missing fixture %s", s.Name, dir)
		}
	}
}

func TestLoadScenariosRejectsBadInput(t *testing.T) {
	// Every case carries a valid outcome assertion, so each one fails for the
	// reason it names rather than for a missing one.
	const outcome = `"answer_contains":["x"]`
	cases := map[string]string{
		"unknown delegate mode": `[{"name":"a","fixture":"notes","prompt":"p",` +
			`"expect":{"delegate":"maybe",` + outcome + `}}]`,
		"duplicate name": `[{"name":"a","fixture":"notes","prompt":"p","expect":{` + outcome + `}},` +
			`{"name":"a","fixture":"notes","prompt":"q","expect":{` + outcome + `}}]`,
		"missing prompt": `[{"name":"a","fixture":"notes","expect":{` + outcome + `}}]`,
		"contradiction": `[{"name":"a","fixture":"notes","prompt":"p",` +
			`"expect":{"delegate":"forbidden","min_parallel":2,` + outcome + `}}]`,
		"parallel but optional": `[{"name":"a","fixture":"notes","prompt":"p",` +
			`"expect":{"min_parallel":2,` + outcome + `}}]`,
		"batched and min_parallel": `[{"name":"a","fixture":"notes","prompt":"p","expect":` +
			`{"delegate":"required","batched":true,"min_parallel":2,` + outcome + `}}]`,
		"unknown agent": `[{"name":"a","fixture":"notes","prompt":"p",` +
			`"expect":{"agents":["scoutt"],` + outcome + `}}]`,
		"name escapes the dir": `[{"name":"../a","fixture":"notes","prompt":"p","expect":{` + outcome + `}}]`,
		"fixture escapes":      `[{"name":"a","fixture":"../../etc","prompt":"p","expect":{` + outcome + `}}]`,
		"changed and unchanged": `[{"name":"a","fixture":"notes","prompt":"p",` +
			`"expect":{"changed":true,"unchanged":true}}]`,
		// A scenario with no outcome assertion is passed by a run that did
		// nothing, which would quietly make the suite reward a lazier model.
		"no outcome assertion": `[{"name":"a","fixture":"notes","prompt":"p","expect":{"delegate":"forbidden"}}]`,
		"empty file":           `[]`,
	}
	for name, body := range cases {
		if _, err := loadFrom(t, body); err == nil {
			t.Errorf("%s: expected a load error", name)
		}
	}

	// The positive control: without it, a rule that rejected everything would
	// pass every case above.
	valid := `[{"name":"a","fixture":"notes","prompt":"p",` +
		`"why":"w","expect":{"delegate":"required","agents":["scout"],` + outcome + `}}]`
	if _, err := loadFrom(t, valid); err != nil {
		t.Errorf("a well-formed scenario was rejected: %v", err)
	}
}

func loadFrom(t *testing.T, body string) ([]Scenario, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadScenarios(path)
}

func TestRatio(t *testing.T) {
	if got := ratio(0, 0); got != "n/a" {
		t.Fatalf("ratio(0,0) = %q, want n/a", got)
	}
	if got := ratio(1, 4); got != "1/4 (25%)" {
		t.Fatalf("ratio(1,4) = %q", got)
	}
}
