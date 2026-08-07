package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/trace"
)

func TestCopyTreeCopiesNestedFiles(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(filepath.Join(src, "services", "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "README.md"), "top")
	write(t, filepath.Join(src, "services", "alpha", "api.go"), "package alpha")

	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(dst, "README.md"):                   "top",
		filepath.Join(dst, "services", "alpha", "api.go"): "package alpha",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}

// Following a symlink would pull whatever it points at into a workspace an
// auto-approving agent then reads. A fixture has no reason to contain one, so
// the copy fails rather than resolving it.
func TestCopyTreeRefusesSymlinks(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "out")
	secret := filepath.Join(t.TempDir(), "secret")
	write(t, secret, "token")
	if err := os.Symlink(secret, filepath.Join(src, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := copyTree(src, dst); err == nil {
		t.Fatal("a symlink in a fixture must fail the copy, not be followed")
	}
	if _, err := os.Stat(filepath.Join(dst, "link")); err == nil {
		t.Fatal("the symlink target was copied into the workspace")
	}
}

func TestCopyTreeRejectsNonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	write(t, file, "x")
	if err := copyTree(file, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("expected an error for a non-directory fixture")
	}
}

// -cwd is what confines an auto-approving agent to this run's copy of the
// fixture. A dropped flag would point it at whatever directory the harness was
// started from.
func TestArgvConfinesTheRun(t *testing.T) {
	h := harness{profile: "workspace-write", temp: 0.3, model: "m", url: "http://x"}
	got := h.argv(Scenario{Prompt: "do it"}, "/tmp/ws", "/tmp/t.jsonl")

	want := map[string]string{
		"-p":                   "do it",
		"-cwd":                 "/tmp/ws",
		"--trace-json":         "/tmp/t.jsonl",
		"--capability-profile": "workspace-write",
		"--model":              "m",
		"--url":                "http://x",
		"--temp":               "0.3",
	}
	for flag, value := range want {
		if !hasFlag(got, flag, value) {
			t.Errorf("argv missing %s %s: %v", flag, value, got)
		}
	}
	if !contains(got, "-y") {
		t.Errorf("argv must auto-approve or every tool call is denied: %v", got)
	}
}

func TestArgvOmitsUnsetPassthroughs(t *testing.T) {
	h := harness{profile: "shell", temp: 1}
	got := h.argv(Scenario{Prompt: "p"}, "d", "t")
	if contains(got, "--model") || contains(got, "--url") {
		t.Fatalf("unset flags must not be passed: %v", got)
	}
}

func hasFlag(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// WriteFile applies its mode only when it creates the file, so a stale 0644
// results file would keep stderr tails and tool arguments world-readable.
func TestWriteResultsTightensAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.json")
	write(t, path, "[]")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeResults(path, []Run{{Scenario: "x", Err: "boom: bash {\"cmd\":\"...\"}"}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("results file mode is %v, want 0600", mode)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "boom") {
		t.Fatalf("results were not written: %s", body)
	}
}

// report is the harness's entire human-facing output; a swapped denominator
// here would misreport every eval with nothing to catch it.
func TestReportShowsRatesAndFailures(t *testing.T) {
	need := Scenario{Name: "need", Fixture: "notes", Prompt: "p",
		Expect: Expect{Delegate: delegateRequired, MinParallel: 2}}
	avoid := Scenario{Name: "avoid", Fixture: "notes", Prompt: "p",
		Expect: Expect{Delegate: delegateForbidden}}
	scenarios := []Scenario{need, avoid}

	good := ScoreRun(need, 1, []trace.Event{
		batch(1, "task", "task"),
		startedBy(callID(1, 0), 1, "scout", "a"), startedBy(callID(1, 1), 1, "scout", "b"),
		{Kind: trace.KindUsage, Tokens: 1200, Round: 1},
	})
	bad := ScoreRun(avoid, 1, []trace.Event{batch(1, "task"), started(1, "scout", "a")})

	var out strings.Builder
	report(&out, scenarios, []Run{good, bad}, "workspace-write")
	got := out.String()

	for _, want := range []string{
		"profile: workspace-write",
		"need",
		"avoid",
		"FAIL avoid #1",
		"delegation recall    1/1 (100%)",
		"delegation precision 0/1 (0%)",
		"parallel compliance  1/1 (100%)",
		"1200",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q\n---\n%s", want, got)
		}
	}
}

// Nothing else crosses the recorder-to-scorer seam: every other test builds
// trace.Event literals, so a change to the recorded shape would show up as a
// perfect score rather than a failure.
func TestRecordedTraceScoresEndToEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	rec, err := trace.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.Start("review alpha and beta", "test-model")
	ev := rec.Wrap(agent.Events{})
	ev.OnToolBatch(1, []agent.ToolCallRef{{ID: "a", Name: "task"}, {ID: "b", Name: "task"}})
	ev.OnAgentStart("a", "scout", "Read services/alpha and list its routes.")
	ev.OnAgentStart("b", "scout", "Read services/beta and list its routes.")
	ev.OnAgentEnd("a", "alpha done", nil)
	ev.OnAgentEnd("b", "beta done", nil)
	rec.End("both reviewed", nil)
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	events, err := trace.Parse(f)
	if err != nil {
		t.Fatal(err)
	}

	s := Scenario{Name: "x", Expect: Expect{Delegate: delegateRequired, MinParallel: 2, Agents: []string{"scout"}}}
	r := ScoreRun(s, 1, events)
	if !r.Passed() {
		t.Fatalf("a real recorded parallel delegation must pass: %v", r.Failures)
	}
	if r.TaskCalls != 2 || r.Delegations != 2 || r.MaxParallel != 2 {
		t.Fatalf("task_calls=%d delegations=%d max_parallel=%d, want 2/2/2",
			r.TaskCalls, r.Delegations, r.MaxParallel)
	}
}
