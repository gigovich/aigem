package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/config"
)

// Delegate expectations. A scenario declares which of the three it is, because
// "should have delegated" and "should have stayed in the main agent" are both
// real failures and a single pass rate would hide the second one.
const (
	delegateRequired  = "required"
	delegateForbidden = "forbidden"
	delegateOptional  = "optional"
)

// Expect is what a run's trace has to show. Zero values mean "no constraint",
// so a scenario only states what it actually measures.
type Expect struct {
	// Delegate is required, forbidden, or optional (default).
	Delegate string `json:"delegate"`
	// Agents lists the acceptable agent types. Checked whenever the run
	// delegates, including under delegate:optional - picking scout to write code
	// is wrong regardless of whether delegating at all was required.
	Agents []string `json:"agents,omitempty"`
	// MinParallel is how many subagents must run together from one assistant
	// message. This is the only check that distinguishes real parallelism from N
	// delegations run one after another. It obliges the run to delegate, so it
	// belongs only on a task where delegating is the right call anyway.
	MinParallel int `json:"min_parallel,omitempty"`
	// Batched asks the weaker question, for a task where delegating is a
	// judgment call: IF the run delegates more than once, the calls have to go
	// out together. Measured on small packages, mandating one subagent per
	// target cost more context than reading them directly, so how a model
	// delegates is worth scoring where whether it should is not.
	Batched bool `json:"batched,omitempty"`
	// MaxTasks caps how many task calls the run may issue, catching a model that
	// shards a small job into a swarm of subagents.
	MaxTasks int `json:"max_tasks,omitempty"`

	// The fields below assert that the work happened at all. Without them a
	// scenario that forbids delegating passes on a run that read nothing and
	// answered "I don't know" - and "delegation precision" would be a metric
	// maximized by doing less.

	// AnswerContains are substrings the final answer must have, matched
	// case-insensitively.
	AnswerContains []string `json:"answer_contains,omitempty"`
	// FileContains maps a workspace-relative path to text that must be present in
	// it once the run finishes; FileAbsent to text that must be gone.
	FileContains map[string]string `json:"file_contains,omitempty"`
	FileAbsent   map[string]string `json:"file_absent,omitempty"`
	// Unchanged asserts the run left every file as it found it, for a prompt that
	// explicitly asked for no edits. Changed is its opposite, for a refactor
	// whose correct result is not one fixed string.
	Unchanged bool `json:"unchanged,omitempty"`
	Changed   bool `json:"changed,omitempty"`
}

// assertsOutcome reports whether the scenario checks that work actually
// happened, rather than only how the model went about it.
func (e Expect) assertsOutcome() bool {
	return len(e.AnswerContains) > 0 || len(e.FileContains) > 0 || len(e.FileAbsent) > 0 ||
		e.Unchanged || e.Changed
}

// Scenario is one prompt run against one fixture workspace.
type Scenario struct {
	Name    string `json:"name"`
	Fixture string `json:"fixture"`
	Prompt  string `json:"prompt"`
	// Why records what the scenario is meant to catch, so a later reader can
	// judge whether the expectation is still the right one.
	Why    string `json:"why"`
	Expect Expect `json:"expect"`
}

func (s Scenario) delegateMode() string {
	if s.Expect.Delegate == "" {
		return delegateOptional
	}
	return s.Expect.Delegate
}

// LoadScenarios reads a JSON array of scenarios and validates it. A typo in
// `delegate` would otherwise silently degrade to "no constraint" and the suite
// would report a pass it never checked.
func LoadScenarios(path string) ([]Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Scenario
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// A misspelled agent name would show up as every run of that scenario
	// failing, which reads as a model problem rather than a scenario typo. The
	// user's own agents dir is loaded too, since that is the set the aigem the
	// runner spawns will offer.
	agentNames := availableAgents()
	known := map[string]bool{}
	for _, n := range agentNames {
		known[n] = true
	}
	seen := map[string]bool{}
	for i, s := range out {
		switch {
		case s.Name == "":
			return nil, fmt.Errorf("%s: scenario %d has no name", path, i)
		case seen[s.Name]:
			return nil, fmt.Errorf("%s: duplicate scenario name %q", path, s.Name)
		case s.Prompt == "":
			return nil, fmt.Errorf("%s: scenario %q has no prompt", path, s.Name)
		case s.Fixture == "":
			return nil, fmt.Errorf("%s: scenario %q has no fixture", path, s.Name)
		}
		seen[s.Name] = true
		switch s.delegateMode() {
		case delegateRequired, delegateForbidden, delegateOptional:
		default:
			return nil, fmt.Errorf("%s: scenario %q has unknown delegate %q (want %s, %s or %s)",
				path, s.Name, s.Expect.Delegate, delegateRequired, delegateForbidden, delegateOptional)
		}
		// min_parallel is a statement about delegations that must happen, so it
		// only means something when delegating is required. Under optional it
		// would fail every run that legitimately took the other branch.
		if s.Expect.MinParallel > 0 && s.delegateMode() != delegateRequired {
			return nil, fmt.Errorf("%s: scenario %q sets min_parallel but delegate is %q, not %s",
				path, s.Name, s.delegateMode(), delegateRequired)
		}
		// batched is the form for a task where delegating is a judgment call, so
		// pairing it with min_parallel means one of the two was meant.
		if s.Expect.Batched && s.Expect.MinParallel > 0 {
			return nil, fmt.Errorf("%s: scenario %q sets both batched and min_parallel; "+
				"min_parallel already requires them batched", path, s.Name)
		}
		for _, a := range s.Expect.Agents {
			if !known[a] {
				return nil, fmt.Errorf("%s: scenario %q expects unknown agent %q (available: %s)",
					path, s.Name, a, strings.Join(agentNames, ", "))
			}
		}
		// A scenario that only says "do not delegate" is passed by a run that did
		// nothing at all, which is the one way this suite could reward a worse
		// model.
		if s.Expect.Unchanged && s.Expect.Changed {
			return nil, fmt.Errorf("%s: scenario %q asserts both changed and unchanged", path, s.Name)
		}
		if !s.Expect.assertsOutcome() {
			return nil, fmt.Errorf("%s: scenario %q asserts nothing about the outcome; add "+
				"answer_contains, file_contains, file_absent or unchanged", path, s.Name)
		}
		// Both halves are joined into filesystem paths, one of them per run.
		if err := safePathElement(s.Name); err != nil {
			return nil, fmt.Errorf("%s: scenario name %q: %w", path, s.Name, err)
		}
		if err := safePathElement(s.Fixture); err != nil {
			return nil, fmt.Errorf("%s: scenario %q fixture %q: %w", path, s.Name, s.Fixture, err)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no scenarios", path)
	}
	return out, nil
}

// availableAgents mirrors what aigem itself registers: the built-ins plus any
// custom definitions in the user's config. A load failure there is not fatal -
// the built-ins alone are still a useful check.
func availableAgents() []string {
	reg := agent.DefaultSubagents()
	if dir, err := config.AgentsDir(); err == nil {
		_ = agent.LoadSubagentsInto(reg, dir)
	}
	return reg.Names()
}

// safePathElement rejects anything that would not stay put when joined to a
// directory. Scenario files are hand-written, so this catches a typo rather
// than an attack, but the runner does copy fixtures and then point an
// auto-approving agent at the result.
func safePathElement(s string) error {
	if strings.ContainsAny(s, `/\`) || strings.Contains(s, "..") {
		return errors.New(`must not contain "/", "\" or ".."`)
	}
	if s != strings.TrimSpace(s) {
		return errors.New("must not have leading or trailing whitespace")
	}
	return nil
}
