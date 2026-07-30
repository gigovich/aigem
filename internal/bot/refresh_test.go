package bot

import (
	"context"
	"testing"

	"github.com/gigovich/aigem/internal/agent"
)

type fakeAgent struct {
	system   string
	lastRun  string
	runCalls int
}

func (f *fakeAgent) SetSystem(s string) { f.system = s }
func (f *fakeAgent) Run(_ context.Context, input string, _ agent.Events) (string, error) {
	f.runCalls++
	f.lastRun = input
	return "ran:" + input + "|sys:" + f.system, nil
}

func TestRefreshingRunnerRebuildsBeforeRun(t *testing.T) {
	fa := &fakeAgent{}
	n := 0
	rr := RefreshingRunner{Agent: fa, Build: func() string {
		n++
		return "sys-v" + string(rune('0'+n))
	}}
	out, err := rr.Run(context.Background(), "hello", agent.Events{})
	if err != nil {
		t.Fatal(err)
	}
	if fa.system != "sys-v1" {
		t.Fatalf("SetSystem not called before Run; system=%q", fa.system)
	}
	if out != "ran:hello|sys:sys-v1" {
		t.Fatalf("unexpected out %q", out)
	}
	if fa.runCalls != 1 {
		t.Fatalf("expected Run called once, got %d", fa.runCalls)
	}
	// A second message rebuilds again (fresh memory index each turn).
	if _, err := rr.Run(context.Background(), "again", agent.Events{}); err != nil {
		t.Fatal(err)
	}
	if fa.system != "sys-v2" {
		t.Fatalf("second Run did not rebuild; system=%q", fa.system)
	}
	if fa.runCalls != 2 {
		t.Fatalf("expected Run called twice total, got %d", fa.runCalls)
	}
}
