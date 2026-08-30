package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/hooks"
	"github.com/gigovich/aigem/internal/llm"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

type fakeActivation struct {
	approved, disallowed []string
	hooks                map[string][]hooks.Matcher
}

func (f *fakeActivation) Approve(t []string)  { f.approved = append(f.approved, t...) }
func (f *fakeActivation) Disallow(t []string) { f.disallowed = append(f.disallowed, t...) }
func (f *fakeActivation) AddHooks(cfg map[string][]hooks.Matcher) {
	if f.hooks == nil {
		f.hooks = map[string][]hooks.Matcher{}
	}
	for e, m := range cfg {
		f.hooks[e] = append(f.hooks[e], m...)
	}
}

func writeSkillFile(t *testing.T, cwd, name, content string) {
	t.Helper()
	dir := filepath.Join(cwd, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillToolReturnsBodyAndPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	writeSkillFile(t, cwd, "fmtcheck",
		"---\nname: fmtcheck\ndescription: format the code\nallowed-tools: Read Bash\ndisallowed-tools: write_file\n---\nRun gofmt then report.\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}

	skills, _ := skill.Discover(cwd)
	reg, _ := tools.NewRegistry(cwd)
	st := NewSkillTool(skills, &fakeClient{}, reg, 0.3, nil)
	if st == nil {
		t.Fatal("expected a skill tool")
	}

	act := &fakeActivation{}
	ctx := WithActivation(context.Background(), act)
	out, err := st.Run(ctx, json.RawMessage(`{"name":"fmtcheck","arguments":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Run gofmt then report.") {
		t.Fatalf("expected skill body, got %q", out)
	}
	if len(act.approved) != 1 || act.approved[0] != "Read" {
		t.Fatalf("expected non-bash allowed-tools approved, got %v", act.approved)
	}
	if len(act.disallowed) != 1 || act.disallowed[0] != "write_file" {
		t.Fatalf("expected disallowed-tools, got %v", act.disallowed)
	}

	if _, err := st.Run(ctx, json.RawMessage(`{"name":"nope"}`)); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}

func TestPathActivationHint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, _ := tools.NewRegistry(dir)
	ag := New(&fakeClient{}, reg, 0.3, nil, "")
	ag.WatchSkills([]*skill.Skill{{Name: "gohelp", Paths: []string{"*.go"}}})
	ag.activated = map[string]bool{}

	tc := llm.ToolCall{ID: "1", Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"foo.go"}`}}
	res := ag.runToolCall(context.Background(), tc, "call-test", Events{})
	if !strings.Contains(res, "gohelp") || !strings.Contains(res, "skill now relevant") {
		t.Fatalf("expected path-activation hint, got:\n%s", res)
	}
	// Only fires once per turn.
	res2 := ag.runToolCall(context.Background(), tc, "call-test", Events{})
	if strings.Contains(res2, "skill now relevant") {
		t.Fatal("hint should fire only once per turn")
	}
}

func TestSkillToolSeesRegistryReplacement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	empty, _ := skill.DiscoverDir(filepath.Join(cwd, ".skills"))
	reg, _ := tools.NewRegistry(cwd)
	st := newSkillTool(empty, &fakeClient{}, reg, 0.3, nil)
	if st == nil {
		t.Fatal("expected a skill tool for an initially empty registry")
	}
	writeSkillFile(t, cwd, "newskill",
		"---\nname: newskill\ndescription: created after agent startup\n---\nFresh body.\n")
	fresh, _ := skill.DiscoverDir(filepath.Join(cwd, ".skills"))
	empty.Replace(fresh)
	if !strings.Contains(st.Description(), "newskill") {
		t.Fatalf("description did not refresh: %s", st.Description())
	}
	if !strings.Contains(string(st.Schema()), "newskill") {
		t.Fatalf("schema did not refresh: %s", st.Schema())
	}
	out, err := st.Run(context.Background(), json.RawMessage(`{"name":"newskill"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Fresh body.") {
		t.Fatalf("invocation did not use refreshed skill: %q", out)
	}
}

func TestSkillToolTracksSaveUpdateDeleteAndMalformed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := t.TempDir()
	root := filepath.Join(base, ".skills")
	reg, _ := skill.DiscoverDir(root)
	toolsReg, _ := tools.NewRegistry(t.TempDir())
	st := newSkillTool(reg, &fakeClient{}, toolsReg, 0.3, nil)
	if st == nil {
		t.Fatal("expected a stable skill tool")
	}
	write := func(description, body string) {
		t.Helper()
		writeSkillFile(t, base, "saved", "---\nname: saved\ndescription: "+description+"\n---\n"+body+"\n")
	}
	refresh := func() {
		t.Helper()
		fresh, _ := skill.DiscoverDir(root)
		reg.Replace(fresh)
	}
	assertConsistent := func(name, body, listing string) {
		t.Helper()
		if !strings.Contains(st.Description(), listing) || !strings.Contains(string(st.Schema()), name) {
			t.Fatalf("tool catalog omitted %q: description=%q schema=%s", name, st.Description(), st.Schema())
		}
		out, err := st.Run(context.Background(), json.RawMessage(fmt.Sprintf(`{"name":%q}`, name)))
		if err != nil || !strings.Contains(out, body) {
			t.Fatalf("invocation for %q = %q, err=%v; want body %q", name, out, err, body)
		}
	}

	if strings.Contains(st.Description(), "saved") || strings.Contains(string(st.Schema()), "saved") {
		t.Fatal("skill must be absent before save")
	}
	write("v1 listing", "v1 body")
	refresh()
	assertConsistent("saved", "v1 body", "v1 listing")

	write("v2 listing", "v2 body")
	refresh()
	assertConsistent("saved", "v2 body", "v2 listing")

	if err := os.RemoveAll(filepath.Join(root, "saved")); err != nil {
		t.Fatal(err)
	}
	refresh()
	if strings.Contains(st.Description(), "saved") || strings.Contains(string(st.Schema()), "saved") {
		t.Fatal("deleted skill remained in the catalog")
	}
	if _, err := st.Run(context.Background(), json.RawMessage(`{"name":"saved"}`)); err == nil || !strings.Contains(err.Error(), "unknown skill") {
		t.Fatalf("deleted skill invocation error = %v", err)
	}

	writeSkillFile(t, base, "broken", "not frontmatter")
	refresh()
	if strings.Contains(st.Description(), "broken") || strings.Contains(string(st.Schema()), "broken") {
		t.Fatal("malformed skill appeared in one of the tool surfaces")
	}
}

func TestSkillToolHidesUserOnlySkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cwd := t.TempDir()
	writeSkillFile(t, cwd, "deploy",
		"---\nname: deploy\ndescription: deploy\ndisable-model-invocation: true\n---\nDeploy.\n")

	skills, _ := skill.Discover(cwd)
	reg, _ := tools.NewRegistry(cwd)
	if st := NewSkillTool(skills, &fakeClient{}, reg, 0.3, nil); st != nil {
		t.Fatal("a registry of only user-only skills must yield no skill tool")
	}
}
