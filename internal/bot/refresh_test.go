package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/skill"
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

func TestRefreshingRunnerDiscoversSkillsPerTurn(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		root := filepath.Join(dir, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("alpha", "---\nname: alpha\ndescription: alpha v1\n---\nbody\n")
	registry, _ := skill.DiscoverDir(dir)
	fa := &fakeAgent{}
	rr := RefreshingRunner{Agent: fa, Build: func() string {
		fresh, _ := skill.DiscoverDir(dir)
		registry.Replace(fresh)
		return registry.Prompt()
	}}

	assert := func(wantPresent bool, wantText string) {
		t.Helper()
		if _, err := rr.Run(context.Background(), "turn", agent.Events{}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(fa.system, wantText) != wantPresent {
			t.Fatalf("skill %q presence=%v in prompt %q", wantText, wantPresent, fa.system)
		}
	}
	assert(true, "alpha v1")
	write("alpha", "---\nname: alpha\ndescription: alpha v2\n---\nbody\n")
	assert(true, "alpha v2")
	if err := os.RemoveAll(filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	assert(false, "alpha v2")
	write("broken", "not skill markdown")
	assert(false, "broken")
}

type refreshingImageAgent struct {
	fakeAgent
	images int
}

func (a *refreshingImageAgent) RunWithImages(_ context.Context, input string, images []llm.Image,
	_ agent.Events) (string, error) {
	a.images = len(images)
	return "ran-image:" + input + "|sys:" + a.system, nil
}

func TestRefreshingRunnerRebuildsBeforeImageRun(t *testing.T) {
	ag := &refreshingImageAgent{}
	rr := RefreshingRunner{Agent: ag, Build: func() string { return "image-system" }}
	out, err := rr.RunWithImages(context.Background(), "screenshot", []llm.Image{{MediaType: "image/png"}}, agent.Events{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "ran-image:screenshot|sys:image-system" || ag.images != 1 {
		t.Fatalf("image run = %q, images=%d", out, ag.images)
	}
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
