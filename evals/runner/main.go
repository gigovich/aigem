// Command runner scores how well the model uses subagent delegation.
//
// It runs each scenario through `aigem -p --trace-json` against a throwaway
// copy of a fixture workspace, then reads the trace back and checks it against
// what the scenario expects. Sampling is noisy, so -n repeats every scenario
// and the report is rates, not verdicts.
//
//	go run ./evals/runner -bin bin/aigem -model <ref> -n 3
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/trace"
)

// Exit codes: 1 means scenarios failed, 2 means the harness could not run them.
const (
	exitFailures = 1
	exitBroken   = 2
)

func main() { os.Exit(run()) }

// run holds the whole harness so its cleanup runs; os.Exit in here would skip
// the deferred removal of the temp workspaces.
func run() int {
	bin := flag.String("bin", "bin/aigem", "aigem binary to exercise")
	scenariosPath := flag.String("scenarios", "evals/scenarios.json", "scenario definitions")
	// Fixtures live under testdata/ so the Go tool ignores them: they are sample
	// projects for the model to work on, not packages of this module.
	fixtures := flag.String("fixtures", "evals/testdata", "fixture workspaces")
	repeats := flag.Int("n", 3, "runs per scenario (sampling is noisy; 1 tells you little)")
	filter := flag.String("filter", "", "only run scenarios whose name contains this")
	model := flag.String("model", "", "model ref passed through to aigem")
	url := flag.String("url", "", "backend URL passed through to aigem")
	temp := flag.Float64("temp", 0.3, "sampling temperature")
	profile := flag.String("capability-profile", "workspace-write",
		"tool envelope for the runs; workspace-write withholds the shell (runs can still reach the network)")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-run wall clock")
	jobs := flag.Int("jobs", 1, "scenario runs in flight (raise only if the provider tolerates it)")
	outPath := flag.String("json", "", "also write the per-run results here")
	keep := flag.Bool("keep", false, "keep the temp workspaces and traces for inspection")
	flag.Parse()

	scenarios, err := LoadScenarios(*scenariosPath)
	if err != nil {
		return fail(err)
	}
	if *filter != "" {
		var kept []Scenario
		for _, s := range scenarios {
			if strings.Contains(s.Name, *filter) {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			return fail(fmt.Errorf("no scenario matches %q", *filter))
		}
		scenarios = kept
	}
	if _, err := os.Stat(*bin); err != nil {
		return fail(fmt.Errorf("%s: %w (run `make build` first)", *bin, err))
	}
	if *repeats < 1 || *jobs < 1 {
		return fail(errors.New("-n and -jobs must be at least 1"))
	}

	work, err := os.MkdirTemp("", "aigem-eval-")
	if err != nil {
		return fail(err)
	}
	if *keep {
		fmt.Fprintln(os.Stderr, "workspaces and traces:", work)
	} else {
		defer os.RemoveAll(work)
	}

	h := harness{
		bin: *bin, fixtures: *fixtures, work: work, model: *model, url: *url,
		temp: *temp, profile: *profile, timeout: *timeout,
	}
	runs := h.runAll(scenarios, *repeats, *jobs)
	report(os.Stdout, scenarios, runs, *profile)

	if *outPath != "" {
		if err := writeResults(*outPath, runs); err != nil {
			return fail(err)
		}
	}
	// A harness failure is not a delegation regression, and reporting it as one
	// sends the reader to the prompt instead of to the backend.
	broken, failed := false, false
	for _, r := range runs {
		broken = broken || r.Harness
		failed = failed || !r.Passed()
	}
	switch {
	case broken:
		return exitBroken
	case failed:
		return exitFailures
	}
	return 0
}

type harness struct {
	bin, fixtures, work string
	model, url, profile string
	temp                float64
	timeout             time.Duration
}

func (h harness) runAll(scenarios []Scenario, repeats, jobs int) []Run {
	type job struct {
		s       Scenario
		attempt int
	}
	var queue []job
	for _, s := range scenarios {
		for i := 1; i <= repeats; i++ {
			queue = append(queue, job{s, i})
		}
	}

	runs := make([]Run, len(queue))
	var wg sync.WaitGroup
	sem := make(chan struct{}, jobs)
	var mu sync.Mutex
	done := 0
	for i, j := range queue {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			runs[i] = h.runOne(j.s, j.attempt)
			mu.Lock()
			done++
			status := "ok"
			if !runs[i].Passed() {
				status = "FAIL"
			}
			fmt.Fprintf(os.Stderr, "[%d/%d] %s #%d %s\n", done, len(queue), j.s.Name, j.attempt, status)
			mu.Unlock()
		}(i, j)
	}
	wg.Wait()
	return runs
}

// argv is the command line for one run. -cwd is what confines an auto-approving
// agent to this run's own copy of the fixture, so it is not optional.
func (h harness) argv(s Scenario, dir, tracePath string) []string {
	args := []string{"-p", s.Prompt, "-cwd", dir, "-y",
		"--capability-profile", h.profile, "--trace-json", tracePath,
		"--temp", fmt.Sprintf("%g", h.temp)}
	if h.model != "" {
		args = append(args, "--model", h.model)
	}
	if h.url != "" {
		args = append(args, "--url", h.url)
	}
	return args
}

// runOne executes a scenario once. Every run gets a fresh copy of the fixture,
// so an edit made by one run cannot change what the next one sees.
func (h harness) runOne(s Scenario, attempt int) Run {
	slug := fmt.Sprintf("%s-%d", s.Name, attempt)
	dir := filepath.Join(h.work, slug)
	if err := copyTree(filepath.Join(h.fixtures, s.Fixture), dir); err != nil {
		return brokenRun(s, attempt, "fixture: "+err.Error())
	}
	before, err := TreeDigest(dir)
	if err != nil {
		return brokenRun(s, attempt, "could not read the fixture copy: "+err.Error())
	}
	tracePath := filepath.Join(h.work, slug+".jsonl")

	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, h.argv(s, dir, tracePath)...)
	// Killing aigem does not kill a grandchild it spawned (a shell command under
	// the shell profile), and cmd.Run waits on the inherited stderr pipe. Without
	// this, one hung command hangs the whole suite past its timeout.
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout = io.Discard
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	f, err := os.Open(tracePath)
	if err != nil {
		// No trace at all means the binary never got as far as the first model
		// call; the stderr tail is the only diagnosis available.
		return brokenRun(s, attempt, fmt.Sprintf("run produced no trace: %v: %s",
			runErr, tail(stderr.String())))
	}
	defer f.Close()
	events, err := trace.Parse(f)
	if err != nil {
		return brokenRun(s, attempt, "unreadable trace: "+err.Error())
	}

	r := ScoreRun(s, attempt, events)
	if runErr != nil && r.Err == "" {
		r.Err = fmt.Sprintf("%v: %s", runErr, tail(stderr.String()))
		r.Failures = append(r.Failures, "run failed")
	}
	CheckOutcome(s, &r, dir, before)
	return r
}

// writeResults saves the per-run results 0600, like the traces: a failed run
// carries its stderr tail, which in -p mode is tool names with their arguments.
// The Chmod covers a path that already existed, where WriteFile's mode is
// ignored and a stale 0644 file would stay world-readable.
func writeResults(path string, runs []Run) error {
	data, err := json.MarshalIndent(runs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// brokenRun is a failure of the harness rather than of the model, which is a
// different exit code and no evidence about delegation either way.
func brokenRun(s Scenario, attempt int, msg string) Run {
	return Run{Scenario: s.Name, Attempt: attempt, Harness: true, Err: msg, Failures: []string{msg}}
}

func report(w io.Writer, scenarios []Scenario, runs []Run, profile string) {
	byScenario := map[string][]Run{}
	for _, r := range runs {
		byScenario[r.Scenario] = append(byScenario[r.Scenario], r)
	}

	fmt.Fprintf(w, "\nprofile: %s\n", profile)
	// A custom SYSTEM.md replaces the built-in base prompt. The delegation block
	// is appended to whichever base is in effect, so it is still being measured -
	// but the surrounding instructions are the user's, and attributing the result
	// to the shipped prompt would be wrong.
	if config.SystemPrompt() != config.DefaultSystemPrompt {
		fmt.Fprintln(w, "NOTE: a custom SYSTEM.md is in effect - the base prompt scored here is")
		fmt.Fprintln(w, "      yours, not the built-in one. The delegation block is appended to")
		fmt.Fprintln(w, "      both, so it is present either way.")
	}

	// Mixed aggregations, so the header says which is which: means per run,
	// except maxbatch, where the widest batch across runs is the interesting one.
	fmt.Fprintf(w, "\n%-26s %5s %9s %8s %9s %9s\n",
		"scenario", "pass", "tasks avg", "maxbatch", "trnds avg", "peaktok avg")
	for _, s := range scenarios {
		rs := byScenario[s.Name]
		if len(rs) == 0 {
			continue
		}
		pass, deleg, batch, rounds, tokens := 0, 0, 0, 0, 0
		for _, r := range rs {
			if r.Passed() {
				pass++
			}
			deleg += r.TaskCalls
			rounds += r.ToolRounds
			tokens += r.PeakTokens
			if r.MaxBatch > batch {
				batch = r.MaxBatch
			}
		}
		n := len(rs)
		fmt.Fprintf(w, "%-26s %2d/%-2d %9.1f %8d %9.1f %9d\n", s.Name, pass, n,
			float64(deleg)/float64(n), batch, float64(rounds)/float64(n), tokens/n)
	}

	for _, s := range scenarios {
		for _, r := range byScenario[s.Name] {
			for _, f := range r.Failures {
				fmt.Fprintf(w, "\n  FAIL %s #%d: %s", s.Name, r.Attempt, f)
			}
		}
	}
	if len(runs) > 0 {
		fmt.Fprintln(w)
	}

	sum := Summarize(scenarios, runs)
	fmt.Fprintf(w, "\nruns                 %s\n", ratio(sum.Pass, sum.Runs))
	fmt.Fprintf(w, "delegation recall    %s   (scenarios that need a subagent)\n",
		ratio(sum.RecallPass, sum.RecallTotal))
	fmt.Fprintf(w, "delegation precision %s   (scenarios that must NOT delegate)\n",
		ratio(sum.PrecisionPass, sum.PrecisionTotal))
	fmt.Fprintf(w, "agent-type accuracy  %s\n", ratio(sum.AgentPass, sum.AgentTotal))
	fmt.Fprintf(w, "parallel compliance  %s   (subagents running together, not in sequence)\n",
		ratio(sum.ParallelPass, sum.ParallelTotal))
	fmt.Fprintf(w, "self-contained (heuristic) %s of delegated prompts free of context references\n",
		ratio(sum.TotalPrompts-sum.VaguePrompts, sum.TotalPrompts))
	// Printed next to the rates, because a model that stops doing the work will
	// otherwise look like one that learned not to over-delegate.
	if sum.OutcomeFailures > 0 {
		fmt.Fprintf(w, "runs that skipped the work %d   (excluded from the rates above)\n", sum.OutcomeFailures)
	}
	if sum.Errors > 0 {
		fmt.Fprintf(w, "runs that errored    %d\n", sum.Errors)
	}
	if sum.TruncatedRuns > 0 {
		fmt.Fprintf(w, "runs cut off by a budget %d   (not evidence either way)\n", sum.TruncatedRuns)
	}
}

// copyTree copies a fixture into dst. Fixtures are small trees of regular
// files; anything else is a mistake worth reporting rather than skipping.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: fixtures may contain only regular files", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, fi.Mode().Perm())
	})
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if lines := strings.Split(s, "\n"); len(lines) > 3 {
		s = strings.Join(lines[len(lines)-3:], " | ")
	}
	return strings.ReplaceAll(s, "\n", " | ")
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return exitBroken
}
