package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadCheckIsOptionalAndStrict(t *testing.T) {
	dir := t.TempDir()
	if got, err := readCheck(dir); got != "" || err != nil {
		t.Fatalf("no file = %q, %v", got, err)
	}
	cfg := filepath.Join(dir, ".aigem", "project.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"check": " make test "}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readCheck(dir); got != "make test" || err != nil {
		t.Errorf("check = %q, %v", got, err)
	}
	if err := os.WriteFile(cfg, []byte(`{"check": 3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCheck(dir); err == nil {
		t.Error("a malformed project.json was accepted")
	}
}

func TestRunCheckKeepsTheEndOfTheOutputAndTheExitStatus(t *testing.T) {
	ctx := context.Background()
	out, err := runCheck(ctx, t.TempDir(), "yes x | head -c 10000; echo; echo tail-line >&2; exit 2")
	if err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("err = %v, want the exit status", err)
	}
	if len(out) > checkOutput || !strings.Contains(out, "tail-line") {
		t.Errorf("output is %d bytes and ends %q", len(out), out[max(0, len(out)-20):])
	}
	if _, err := runCheck(ctx, t.TempDir(), "true"); err != nil {
		t.Errorf("a passing check = %v", err)
	}
}

func TestACheckThatHangsIsStoppedAtTheBudget(t *testing.T) {
	old := checkBudget
	checkBudget = 300 * time.Millisecond
	t.Cleanup(func() { checkBudget = old })
	began := time.Now()
	out, err := runCheck(context.Background(), t.TempDir(), "echo started; sleep 30 & wait")
	if err == nil || !strings.Contains(err.Error(), "ran past") {
		t.Errorf("err = %v, want the budget named", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("a hanging check took %s to stop", took)
	}
	if !strings.Contains(out, "started") {
		t.Errorf("output = %q, want what it printed before it hung", out)
	}
}
