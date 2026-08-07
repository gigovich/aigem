package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func workspace(t *testing.T, files map[string]string) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before, err := TreeDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, before
}

// Without this gate, a scenario that forbids delegating is passed by a run that
// read nothing and answered "I don't know" - and delegation precision becomes a
// score a lazier model wins.
func TestOutcomeCatchesARunThatDidNothing(t *testing.T) {
	// Assembled rather than written out: the misspell linter would otherwise
	// flag the typo this scenario exists to detect.
	const typo, fixed = "rec" + "ieve", "receive"
	s := Scenario{Name: "x", Expect: Expect{
		Delegate:     delegateForbidden,
		FileContains: map[string]string{"README.md": fixed},
		FileAbsent:   map[string]string{"README.md": typo},
	}}
	dir, before := workspace(t, map[string]string{"README.md": "please " + typo + " this"})

	r := Run{Scenario: s.Name}
	CheckOutcome(s, &r, dir, before)
	if !r.OutcomeFailed || r.Passed() {
		t.Fatalf("an untouched typo must fail the run: %v", r.Failures)
	}

	sum := Summarize([]Scenario{s}, []Run{r})
	if sum.PrecisionPass != 0 || sum.PrecisionTotal != 0 {
		t.Fatalf("a run that skipped the work must stay out of the precision rate: %d/%d",
			sum.PrecisionPass, sum.PrecisionTotal)
	}
	if sum.OutcomeFailures != 1 {
		t.Fatalf("outcome failures = %d, want 1", sum.OutcomeFailures)
	}
}

func TestOutcomePassesWhenTheEditLanded(t *testing.T) {
	const typo, fixed = "rec" + "ieve", "receive"
	s := Scenario{Name: "x", Expect: Expect{
		FileContains: map[string]string{"README.md": fixed},
		FileAbsent:   map[string]string{"README.md": typo},
		Changed:      true,
	}}
	dir, before := workspace(t, map[string]string{"README.md": "please " + typo + " this"})
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("please "+fixed+" this"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := Run{Scenario: s.Name}
	CheckOutcome(s, &r, dir, before)
	if r.OutcomeFailed {
		t.Fatalf("the fixed typo must pass: %v", r.Failures)
	}
}

func TestOutcomeChecksTheAnswer(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{AnswerContains: []string{"flushMaxAttempts"}}}
	dir, before := workspace(t, map[string]string{"flush.go": "package notes"})

	silent := Run{Answer: "It retries a few times."}
	CheckOutcome(s, &silent, dir, before)
	if !silent.OutcomeFailed {
		t.Fatal("an answer that never names the constant must fail")
	}

	// Case-insensitive, since the answer is prose and may reflow the name.
	found := Run{Answer: "The cap is FLUSHMAXATTEMPTS, set to 4."}
	CheckOutcome(s, &found, dir, before)
	if found.OutcomeFailed {
		t.Fatalf("a correct answer must pass: %v", found.Failures)
	}
}

func TestOutcomeDetectsForbiddenEdits(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Unchanged: true}}
	dir, before := workspace(t, map[string]string{"flush.go": "package notes", "sub/store.go": "package notes"})
	if err := os.WriteFile(filepath.Join(dir, "sub", "store.go"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := Run{}
	CheckOutcome(s, &r, dir, before)
	if !r.OutcomeFailed {
		t.Fatal(`"do not change the code" must be enforced, not just scored`)
	}
	if !strings.Contains(strings.Join(r.Failures, " "), filepath.Join("sub", "store.go")) {
		t.Fatalf("the failure must name the edited file: %v", r.Failures)
	}
}

func TestOutcomeDetectsAddedAndRemovedFiles(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Unchanged: true}}
	dir, before := workspace(t, map[string]string{"a.go": "package a", "b.go": "package b"})
	if err := os.Remove(filepath.Join(dir, "b.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "c.go"), []byte("package c"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := Run{}
	CheckOutcome(s, &r, dir, before)
	joined := strings.Join(r.Failures, " ")
	if !strings.Contains(joined, "b.go (removed)") || !strings.Contains(joined, "c.go (added)") {
		t.Fatalf("added and removed files must both be reported: %v", r.Failures)
	}
}

func TestOutcomeReportsAMissingFile(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{FileContains: map[string]string{"gone.go": "x"}}}
	dir, before := workspace(t, map[string]string{"a.go": "package a"})

	r := Run{}
	CheckOutcome(s, &r, dir, before)
	if !r.OutcomeFailed {
		t.Fatal("a missing asserted file must fail rather than pass silently")
	}
}

// A run stopped by a runaway budget ends without an error and with a plausible
// answer, but its behavior after the cut is missing.
func TestTruncatedRunIsNotEvidence(t *testing.T) {
	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateForbidden, AnswerContains: []string{"x"}}}
	r := Run{Scenario: "x", Truncated: true, Answer: "x"}

	sum := Summarize([]Scenario{s}, []Run{r})
	if sum.PrecisionTotal != 0 {
		t.Fatal("a budget-truncated run must not enter the rate denominators")
	}
	if sum.TruncatedRuns != 1 {
		t.Fatalf("truncated = %d, want 1", sum.TruncatedRuns)
	}
}
