package runner_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/runner"
	"github.com/gigovich/aigem/internal/search"
	"github.com/gigovich/aigem/internal/skill"
	"github.com/gigovich/aigem/internal/tools"
)

// project builds a directory Load can be pointed at.
func project(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// writeSkill puts a skill under the project's .skills, which is where discovery
// looks for project-local ones.
func writeSkill(t *testing.T, cwd, name, body string) {
	t.Helper()
	dir := filepath.Join(cwd, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func noticeTexts(ns []runner.Notice) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Text)
	}
	return out
}

func findNotice(ns []runner.Notice, substr string) (runner.Notice, bool) {
	for _, n := range ns {
		if strings.Contains(n.Text, substr) {
			return n, true
		}
	}
	return runner.Notice{}, false
}

func TestLoadDiscoversTrustedProjectSkills(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}

	env, notices := runner.Load(runner.Options{Cwd: cwd})
	t.Cleanup(env.Close)

	if env.Pending != nil {
		t.Fatalf("approved project should have nothing pending, got %+v", env.Pending)
	}
	if got := env.Skills.ModelNames(); len(got) != 1 || got[0] != "greet" {
		t.Fatalf("expected the greet skill to be discovered, got %v (notices %v)",
			got, noticeTexts(notices))
	}
	if p := env.SystemPrompt(nil); !strings.Contains(p, "say hi nicely") {
		t.Fatalf("expected the skill catalog in the system prompt, got %q", p)
	}
}

// Discovery drops untrusted project-local skills silently, which is
// indistinguishable from the project having none. Load has to hand the caller
// what was withheld, or nobody can offer to approve it.
func TestLoadWithholdsUntrustedProjectSkills(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n")

	env, _ := runner.Load(runner.Options{Cwd: cwd})
	t.Cleanup(env.Close)

	if len(env.Skills.ModelNames()) != 0 {
		t.Fatalf("untrusted skills must not be loaded, got %v", env.Skills.ModelNames())
	}
	if env.Pending == nil {
		t.Fatal("expected the withheld skill to be reported as pending")
	}
	if len(env.Pending.Names) != 1 || env.Pending.Names[0] != "greet" {
		t.Fatalf("expected greet to be pending, got %v", env.Pending.Names)
	}
}

// TrustProjectSkills is the --trust-project-skills flag: it approves before
// discovery, so the same call that would have withheld them loads them.
func TestLoadTrustsProjectSkillsOnRequest(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "greet", "---\nname: greet\ndescription: say hi nicely\n---\nSay hello.\n")

	env, notices := runner.Load(runner.Options{Cwd: cwd, TrustProjectSkills: true})
	t.Cleanup(env.Close)

	if len(env.Skills.ModelNames()) != 1 {
		t.Fatalf("expected the skill to be trusted and loaded, got %v (notices %v)",
			env.Skills.ModelNames(), noticeTexts(notices))
	}
	if env.Pending != nil {
		t.Fatalf("nothing should be pending after approval, got %+v", env.Pending)
	}
}

// A project whose settings file will not parse is still a project someone wants
// to work in, so Load reports it and carries on with the hooks it could read.
func TestLoadReportsBrokenHookConfig(t *testing.T) {
	cwd := project(t)
	if err := os.MkdirAll(filepath.Join(cwd, ".aigem"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".aigem", "settings.json"),
		[]byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	env, notices := runner.Load(runner.Options{Cwd: cwd})
	t.Cleanup(env.Close)

	n, ok := findNotice(notices, "hook config:")
	if !ok {
		t.Fatalf("expected a hook config notice, got %v", noticeTexts(notices))
	}
	if n.InChat {
		t.Fatal("a hook config notice is not one the TUI repeats in the transcript")
	}
	if env.Hooks == nil {
		t.Fatal("a broken config must still leave a hooks runner behind")
	}
}

// The caller decides how a notice is presented, so Load must not have decided
// already by baking a prefix into the text.
func TestNoticesCarryNoPrefix(t *testing.T) {
	cwd := project(t)
	writeSkill(t, cwd, "broken", "---\nname: [unterminated\n---\nnothing\n")
	if err := skill.ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}

	_, notices := runner.Load(runner.Options{Cwd: cwd})
	if len(notices) == 0 {
		t.Fatal("expected a notice for a skill that cannot be parsed")
	}
	for _, n := range notices {
		if strings.HasPrefix(n.Text, "warning") {
			t.Fatalf("notice %q carries a presentation prefix", n.Text)
		}
	}
	if n, ok := findNotice(notices, "skipped skill:"); !ok || !n.InChat {
		t.Fatalf("a skipped skill is raised before the alt screen and has to be repeated in it, "+
			"got %+v", notices)
	}
}

// Two conversations must not share a registry: the delegation and skill tools
// are bound to one session's confirmation function.
func TestNewToolsGivesEachSessionItsOwnRegistry(t *testing.T) {
	cwd := project(t)
	env, _ := runner.Load(runner.Options{Cwd: cwd})
	t.Cleanup(env.Close)

	a, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	b, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("NewTools handed out the same registry twice")
	}
	a.Register(markerTool{})
	if _, ok := b.Get("marker"); ok {
		t.Fatal("registering into one session's registry reached another's")
	}
	if a.Root() != b.Root() {
		t.Fatalf("both registries are the same sandbox: %q vs %q", a.Root(), b.Root())
	}
}

func TestNewToolsRegistersSearchOnlyWhenConfigured(t *testing.T) {
	cwd := project(t)

	off, _ := runner.Load(runner.Options{Cwd: cwd})
	t.Cleanup(off.Close)
	reg, err := off.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("web_search"); ok {
		t.Fatal("an unconfigured search backend must not register web_search")
	}

	on, _ := runner.Load(runner.Options{
		Cwd:    cwd,
		Search: search.Config{Provider: "brave", Brave: &search.BraveConfig{APIKey: "k"}},
	})
	t.Cleanup(on.Close)
	reg, err = on.NewTools()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("web_search"); !ok {
		t.Fatalf("expected web_search once search is configured, got %v", reg.Names())
	}
}

// The prompt carries the instruction files, and the session that got them must
// not spend a tool call reading them back.
func TestSystemPromptMarksInstructionFilesInContext(t *testing.T) {
	cwd := project(t)
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("PROJECT RULES\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, _ := runner.Load(runner.Options{Cwd: cwd})
	t.Cleanup(env.Close)
	reg, err := env.NewTools()
	if err != nil {
		t.Fatal(err)
	}

	if out := readFile(t, reg, "AGENTS.md"); !strings.Contains(out, "PROJECT RULES") {
		t.Fatalf("expected the file to read normally before the prompt is built, got %q", out)
	}
	if p := env.SystemPrompt(reg); !strings.Contains(p, "PROJECT RULES") {
		t.Fatalf("expected the project instructions in the prompt, got %q", p)
	}
	if out := readFile(t, reg, "AGENTS.md"); !strings.Contains(out, "already included") {
		t.Fatalf("expected read_file to report the file as already in context, got %q", out)
	}
}

// /new rebuilds the prompt so an edit takes effect without a restart. That only
// works if the files are re-read rather than snapshotted at load.
func TestSystemPromptRereadsInstructionFiles(t *testing.T) {
	cwd := project(t)
	path := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(path, []byte("FIRST RULE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, _ := runner.Load(runner.Options{Cwd: cwd})
	t.Cleanup(env.Close)

	if p := env.SystemPrompt(nil); !strings.Contains(p, "FIRST RULE") {
		t.Fatalf("expected the first version, got %q", p)
	}
	if err := os.WriteFile(path, []byte("SECOND RULE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := env.SystemPrompt(nil)
	if !strings.Contains(p, "SECOND RULE") || strings.Contains(p, "FIRST RULE") {
		t.Fatalf("expected the edited instructions, got %q", p)
	}
	// Project is the load-time snapshot, and is deliberately not re-read: it is
	// what a subagent was told about the project when the session started.
	if !strings.Contains(env.Project, "FIRST RULE") {
		t.Fatalf("expected Project to hold the load-time text, got %q", env.Project)
	}
}

// readFile runs the read_file tool, which is how the in-context marking is
// observable at all.
func readFile(t *testing.T, reg *tools.Registry, path string) string {
	t.Helper()
	tool, ok := reg.Get("read_file")
	if !ok {
		t.Fatal("no read_file tool in the registry")
	}
	args, err := json.Marshal(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatalf("read_file %s: %v", path, err)
	}
	return out
}

// markerTool is a tool with no behaviour, for asserting which registry a
// registration landed in.
type markerTool struct{}

func (markerTool) Name() string            { return "marker" }
func (markerTool) Description() string     { return "marker" }
func (markerTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (markerTool) NeedsConfirm() bool      { return false }
func (markerTool) Run(context.Context, json.RawMessage) (string, error) {
	return "", nil
}
