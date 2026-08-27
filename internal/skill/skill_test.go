package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/gigovich/aigem/internal/config"
	projecttrust "github.com/gigovich/aigem/internal/trust"
)

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolate points HOME, XDG_CONFIG_HOME and XDG_STATE_HOME at empty temp dirs so
// real ~/.claude and ~/.config skills, and real project approvals, stay out of
// discovery tests.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Also the trust store: without it a test would read - and worse, write -
	// the developer's real project approvals.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func TestDiscoverPriorityAndSources(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".skills"), "foo",
		"---\nname: foo\ndescription: from dot-skills\n---\nbody A\n")
	writeSkill(t, filepath.Join(cwd, ".claude", "skills"), "foo",
		"---\nname: foo\ndescription: from claude\n---\nbody B\n")
	writeSkill(t, filepath.Join(cwd, ".claude", "skills"), "bar",
		"---\nname: bar\ndescription: claude only\n---\nbody C\n")
	if err := ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}

	r, errs := Discover(cwd)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	foo, ok := r.Get("foo")
	if !ok || foo.Description != "from dot-skills" {
		t.Fatalf(".skills must shadow .claude/skills, got %+v", foo)
	}
	if _, ok := r.Get("bar"); !ok {
		t.Fatal("claude-only skill should be discovered")
	}
}

func TestParseFrontmatter(t *testing.T) {
	s, err := parse("---\n" +
		"name: deploy\n" +
		"description: ship it\n" +
		"argument-hint: \"[env]\"\n" +
		"allowed-tools: Read, Grep Bash\n" +
		"arguments:\n  - env\n  - tag\n" +
		"disable-model-invocation: true\n" +
		"user-invocable: false\n" +
		"---\nDeploy to $env tag $tag.\n")
	if err != nil {
		t.Fatal(err)
	}
	if s.Description != "ship it" || s.ArgHint != "[env]" {
		t.Fatalf("scalars: %+v", s)
	}
	if len(s.AllowedTools) != 3 || s.AllowedTools[0] != "Read" {
		t.Fatalf("allowed-tools list: %v", s.AllowedTools)
	}
	if len(s.Args) != 2 || s.Args[1] != "tag" {
		t.Fatalf("arguments list: %v", s.Args)
	}
	if !s.DisableModelInvocation || s.UserInvocable {
		t.Fatalf("flags: %+v", s)
	}
	if s.ModelInvocable() {
		t.Fatal("disable-model-invocation should hide from the model")
	}
}

func TestRenderSubstitutionAndDynamic(t *testing.T) {
	s, err := parse("---\nname: t\ndescription: d\narguments: env\n---\n" +
		"env=$env all=$ARGUMENTS dir=${CLAUDE_SKILL_DIR} out=!`printf hi` lit=\\$X\n")
	if err != nil {
		t.Fatal(err)
	}
	s.Dir = t.TempDir()
	got, err := s.Render(context.Background(), "prod v2", RenderOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"env=prod", "all=prod v2", "dir=" + s.Dir, "out=hi", "lit=$X"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderAppendsArgumentsWhenNoPlaceholder(t *testing.T) {
	s, _ := parse("---\nname: t\ndescription: d\n---\nDo the thing.\n")
	got, _ := s.Render(context.Background(), "extra args", RenderOpts{})
	if !strings.Contains(got, "ARGUMENTS: extra args") {
		t.Fatalf("expected appended ARGUMENTS, got:\n%s", got)
	}
}

func TestPathsMatching(t *testing.T) {
	s, _ := parse("---\nname: go\ndescription: d\npaths: '**/*.go'\n---\nb\n")
	if !s.Conditional() {
		t.Fatal("paths skill should be conditional")
	}
	if !s.Matches("internal/agent/agent.go") {
		t.Fatal("should match nested .go")
	}
	if s.Matches("README.md") {
		t.Fatal("should not match .md")
	}
	base, _ := parse("---\nname: md\ndescription: d\npaths: '*.md'\n---\nb\n")
	if !base.Matches("docs/plan.md") {
		t.Fatal("*.md should match by basename")
	}
}

func TestConditionalExcludedFromPrompt(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".skills"), "always",
		"---\nname: always\ndescription: always on\n---\nb\n")
	writeSkill(t, filepath.Join(cwd, ".skills"), "cond",
		"---\nname: cond\ndescription: only for go\npaths: '*.go'\n---\nb\n")
	if err := ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	r, _ := Discover(cwd)
	p := r.Prompt()
	if !strings.Contains(p, "always") || strings.Contains(p, "cond") {
		t.Fatalf("conditional skill must be excluded from the listing:\n%s", p)
	}
	if len(r.Conditional()) != 1 {
		t.Fatalf("expected 1 conditional skill, got %d", len(r.Conditional()))
	}
}

func TestDescriptionFallsBackToFirstParagraph(t *testing.T) {
	s, _ := parse("---\nname: t\n---\nFirst paragraph here.\n\nSecond.\n")
	if s.Description != "First paragraph here." {
		t.Fatalf("description fallback: %q", s.Description)
	}
}

func TestDiscoverSkipsProjectSkillsUntilTrusted(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".skills"), "local",
		"---\nname: local\ndescription: local\n---\nbody\n")

	r, errs := Discover(cwd)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if r.Len() != 0 {
		t.Fatalf("untrusted project skills must not be discovered, got %d", r.Len())
	}
	if err := ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	r, errs = Discover(cwd)
	if len(errs) != 0 {
		t.Fatalf("trusted errors: %v", errs)
	}
	s, ok := r.Get("local")
	if !ok || !s.ProjectLocal {
		t.Fatalf("trusted project skill not discovered/marked project-local: %+v", s)
	}
}

func TestProjectSkillApprovalInvalidatesOnDefinitionChange(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".skills")
	writeSkill(t, root, "local", "---\nname: local\ndescription: local\n---\nbody v1\n")
	if err := ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	r, errs := Discover(cwd)
	if len(errs) != 0 || r.Len() != 1 {
		t.Fatalf("approved skills: len=%d errs=%v", r.Len(), errs)
	}
	writeSkill(t, root, "local", "---\nname: local\ndescription: local\n---\nbody v2\n")
	r, errs = Discover(cwd)
	if len(errs) != 0 || r.Len() != 0 {
		t.Fatalf("changed skill approval was not invalidated: len=%d errs=%v", r.Len(), errs)
	}
}

func TestDiscoverDir(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "greet")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: greet\ndescription: How to greet users warmly\n---\nSay hello, then ask how you can help."
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, errs := DiscoverDir(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	s, ok := reg.Get("greet")
	if !ok {
		t.Fatal("greet skill not discovered")
	}
	if s.Description != "How to greet users warmly" {
		t.Errorf("description = %q", s.Description)
	}
	if !strings.Contains(s.Body(), "Say hello") {
		t.Errorf("body = %q", s.Body())
	}
	if reg.Prompt() == "" {
		t.Error("catalog Prompt() should be non-empty")
	}
}

func TestDiscoverDirMissing(t *testing.T) {
	reg, errs := DiscoverDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(errs) != 0 || reg.Len() != 0 {
		t.Fatalf("missing dir should yield an empty registry, got len=%d errs=%v", reg.Len(), errs)
	}
}

func TestDiscoverFS(t *testing.T) {
	fsys := fstest.MapFS{
		"greet/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: greet\ndescription: How to greet users warmly\n---\nSay hello.")},
		"unnamed/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\ndescription: Name falls back to the directory\n---\nBody.")},
		"not-a-skill/README.md": &fstest.MapFile{Data: []byte("no SKILL.md here")},
	}
	reg, errs := DiscoverFS(fsys)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if reg.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", reg.Len())
	}
	s, ok := reg.Get("greet")
	if !ok {
		t.Fatal("greet skill not discovered")
	}
	if !s.Builtin {
		t.Error("DiscoverFS skills must be marked Builtin")
	}
	if s.Dir != "" {
		t.Errorf("Dir = %q, want empty for embedded skills", s.Dir)
	}
	if s.Path != "builtin:greet" {
		t.Errorf("Path = %q", s.Path)
	}
	if !strings.Contains(s.Body(), "Say hello") {
		t.Errorf("body = %q", s.Body())
	}
	if _, ok := reg.Get("unnamed"); !ok {
		t.Error("skill without a frontmatter name should default to its directory name")
	}
}

func TestMergeMissing(t *testing.T) {
	builtins, _ := DiscoverFS(fstest.MapFS{
		"shared/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: shared\ndescription: builtin wins\n---\nbuiltin body")},
	})
	dir := t.TempDir()
	writeSkill(t, dir, "shared", "---\nname: shared\ndescription: self-authored\n---\nself body")
	writeSkill(t, dir, "extra", "---\nname: extra\ndescription: self only\n---\nextra body")
	selfSkills, _ := DiscoverDir(dir)

	builtins.MergeMissing(selfSkills)
	if builtins.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", builtins.Len())
	}
	shared, _ := builtins.Get("shared")
	if shared.Description != "builtin wins" {
		t.Errorf("merge must not overwrite an already-claimed name, got %q", shared.Description)
	}
	if _, ok := builtins.Get("extra"); !ok {
		t.Error("unclaimed self-skill should merge in")
	}
}

func TestMergeMissingKeepsSortedOrder(t *testing.T) {
	builtins, _ := DiscoverFS(fstest.MapFS{
		"zeta/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: zeta\ndescription: d\n---\nb")},
	})
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "---\nname: alpha\ndescription: d\n---\nb")
	selfSkills, _ := DiscoverDir(dir)
	builtins.MergeMissing(selfSkills)
	var names []string
	for _, s := range builtins.List() {
		names = append(names, s.Name)
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Fatalf("merged listing must be re-sorted, got %v", names)
	}
}

func TestRegistryRemove(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "keep", "---\nname: keep\ndescription: d\n---\nb")
	writeSkill(t, dir, "drop", "---\nname: drop\ndescription: d\n---\nb")
	reg, _ := DiscoverDir(dir)
	reg.Remove("drop")
	reg.Remove("never-existed")
	if reg.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", reg.Len())
	}
	if _, ok := reg.Get("drop"); ok {
		t.Error("removed skill still retrievable")
	}
	if len(reg.List()) != 1 || reg.List()[0].Name != "keep" {
		t.Errorf("List() = %v", reg.List())
	}
}

func TestDiscoverDirEmptyRoot(t *testing.T) {
	reg, errs := DiscoverDir("")
	if len(errs) != 0 || reg.Len() != 0 {
		t.Fatalf("empty root must yield an empty registry, got len=%d errs=%v", reg.Len(), errs)
	}
}

func TestScanParseErrorPaths(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "bad", "no frontmatter here")
	_, errs := DiscoverDir(dir)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), filepath.Join(dir, "bad", "SKILL.md")) {
		t.Fatalf("disk parse error must cite the SKILL.md path, got %v", errs)
	}
	_, errs = DiscoverFS(fstest.MapFS{
		"bad/SKILL.md": &fstest.MapFile{Data: []byte("no frontmatter here")},
	})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "builtin:bad") {
		t.Fatalf("embedded parse error must cite builtin:<name>, got %v", errs)
	}
}

func TestRenderSkipsDynamicInjectionWithoutDir(t *testing.T) {
	reg, _ := DiscoverFS(fstest.MapFS{
		"dyn/SKILL.md": &fstest.MapFile{Data: []byte(
			"---\nname: dyn\ndescription: d\n---\nOutput: !`echo pwned`")},
	})
	s, _ := reg.Get("dyn")
	got, err := s.Render(context.Background(), "", RenderOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "!`echo pwned`") || strings.Contains(got, "pwned\n") {
		t.Fatalf("builtin skill must not exec dynamic blocks, got %q", got)
	}
}

func TestPendingReportsWithheldProjectSkills(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".skills"), "beta",
		"---\nname: beta\ndescription: beta\n---\nbody\n")
	writeSkill(t, filepath.Join(cwd, ".claude", "skills"), "alpha",
		"---\nname: alpha\ndescription: alpha\n---\nbody\n")

	p, err := Pending(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("untrusted project skills must be reported as pending")
		return
	}
	if p.Invalidated {
		t.Error("a never-approved project is pending, not invalidated")
	}
	if got := strings.Join(p.Names, ","); got != "alpha,beta" {
		t.Errorf("names = %q, want %q", got, "alpha,beta")
	}
	if p.Dir != cwd {
		t.Errorf("dir = %q, want %q", p.Dir, cwd)
	}

	if err := ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	if p, err := Pending(cwd); err != nil || p != nil {
		t.Fatalf("approved project must have nothing pending: %+v %v", p, err)
	}
}

func TestPendingFlagsInvalidatedApproval(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".skills")
	writeSkill(t, root, "local", "---\nname: local\ndescription: local\n---\nbody v1\n")
	if err := ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "local", "---\nname: local\ndescription: local\n---\nbody v2\n")

	p, err := Pending(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || !p.Invalidated {
		t.Fatalf("changed definitions must report as invalidated, got %+v", p)
	}
}

func TestPendingNilWithoutProjectSkills(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(os.Getenv("HOME"), ".claude", "skills"), "global",
		"---\nname: global\ndescription: global\n---\nbody\n")

	p, err := Pending(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatalf("a project with no local skills has nothing pending, got %+v", p)
	}
}

func TestReplaceSwapsContentsInPlace(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".skills"), "local",
		"---\nname: local\ndescription: local\n---\nbody\n")

	r, _ := Discover(cwd)
	if r.Len() != 0 {
		t.Fatalf("precondition: untrusted project skills must be absent, got %d", r.Len())
	}
	if err := ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	fresh, _ := Discover(cwd)
	// The pointer must survive: the skill tool and system-prompt builder hold it.
	held := r
	r.Replace(fresh)
	if held.Len() != 1 {
		t.Fatalf("holders of the old pointer see %d skills, want 1", held.Len())
	}
	if _, ok := held.Get("local"); !ok {
		t.Error("replaced registry must expose the newly trusted skill")
	}
	if got := held.ModelNames(); len(got) != 1 || got[0] != "local" {
		t.Errorf("ModelNames = %v, want [local]", got)
	}
}

func TestReplaceWithEmptyRegistryClears(t *testing.T) {
	r, _ := DiscoverFS(fstest.MapFS{
		"one/SKILL.md": {Data: []byte("---\nname: one\ndescription: one\n---\nbody\n")},
	})
	if r.Len() != 1 {
		t.Fatalf("setup: len = %d", r.Len())
	}
	r.Replace(&Registry{byName: map[string]*Skill{}})
	if r.Len() != 0 || len(r.List()) != 0 {
		t.Fatalf("replace with an empty registry must clear, got %d", r.Len())
	}
	r.Replace(nil)
	if r.Len() != 0 {
		t.Fatalf("replace with nil must clear, got %d", r.Len())
	}
}

func TestPendingNilAfterExplicitRevoke(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".skills"), "local",
		"---\nname: local\ndescription: local\n---\nbody\n")
	fingerprint, sources, err := projectSkillsFingerprint(cwd)
	if err != nil || len(sources) == 0 {
		t.Fatalf("fingerprint: sources=%d err=%v", len(sources), err)
	}
	projectDir := config.ProjectDir(cwd)
	err = projecttrust.Revoke(projectDir, projecttrust.CapabilitySkills, "project-skills", fingerprint, "user")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Pending(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatalf("a revoked project must not be re-offered, got %+v", p)
	}
	if r, _ := Discover(cwd); r.Len() != 0 {
		t.Errorf("revoked project skills must stay withheld, got %d", r.Len())
	}
}

func TestPendingSanitizesUntrustedNames(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	// A hostile repo controls this string; yaml decodes \e to a real escape.
	writeSkill(t, filepath.Join(cwd, ".skills"), "evil",
		"---\nname: \"\\e[2J\\e[Hgit\\tstatus\\u0007\"\ndescription: d\n---\nbody\n")

	p, err := Pending(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || len(p.Names) != 1 {
		t.Fatalf("pending = %+v", p)
	}
	got := p.Names[0]
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
		t.Fatalf("control characters survived into the trust prompt: %q", got)
	}
	if got != "[2J[Hgit status" {
		t.Errorf("name = %q, want the printable remainder", got)
	}
}

func TestDisplaySafeStripsReorderingAndHidingRunes(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		// The escape is what makes the rest a control sequence; the residue is
		// inert printable text and is deliberately left visible.
		{"ansi escape", "a\x1b[31mb", "a[31mb"},
		{"c1 csi byte", "a\u009bb", "ab"},
		{"del", "a\u007fb", "ab"},
		{"bidi override", "safe\u202egnorw", "safegnorw"},
		{"bidi isolate", "a\u2066b\u2069c", "abc"},
		{"zero width", "g\u200bi\u200dt\ufeffea", "gitea"},
		{"soft hyphen", "gi\u00adtea", "gitea"},
		{"tab becomes space", "git\tstatus", "git status"},
		{"newline", "one\ntwo", "onetwo"},
		{"printable kept", "gitea-issues_v2 (beta)", "gitea-issues_v2 (beta)"},
	} {
		if got := DisplaySafe(tc.in); got != tc.want {
			t.Errorf("%s: DisplaySafe(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestPendingKeepsUnprintableNamesVisible(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".skills")
	writeSkill(t, root, "a", "---\nname: \"\\u0001\\u0002\"\ndescription: d\n---\nbody\n")
	writeSkill(t, root, "b", "---\nname: \"\\u0003\"\ndescription: d\n---\nbody\n")
	writeSkill(t, root, "c", "---\nname: real\ndescription: d\n---\nbody\n")

	p, err := Pending(cwd)
	if err != nil || p == nil {
		t.Fatalf("pending = %+v, err = %v", p, err)
	}
	// Sanitizing away a name must not sanitize away the fact that it exists, or
	// a repo could shrink the list the user is asked to approve.
	if len(p.Names) != 3 {
		t.Fatalf("names = %q, want one entry per skill", p.Names)
	}
	// The placeholder names the directory, so two unprintable names stay distinct
	// and the user can still tell which skill is which.
	want := []string{"a (unprintable name)", "b (unprintable name)", "real"}
	for i, w := range want {
		if p.Names[i] != w {
			t.Errorf("names = %q, want %q", p.Names, want)
			break
		}
	}
}

func TestPendingNonEmptyWhenDefinitionsUnparsable(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".skills"), "broken", "no frontmatter here\n")

	p, err := Pending(cwd)
	if err != nil {
		t.Fatal(err)
	}
	// The files exist and are withheld, so the user must still be told - even
	// though nothing could be parsed to name.
	if p == nil {
		t.Fatal("unparsable project skills must still be reported as pending")
		return
	}
	// The file exists and is covered by the approval, so it must be listed even
	// though nothing could be parsed to name it.
	if len(p.Names) != 1 || !strings.Contains(p.Names[0], "unparsable") {
		t.Errorf("names = %v, want the directory flagged unparsable", p.Names)
	}
}

func TestIndependentRegistriesRefreshConcurrently(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	writeSkill(t, left, "left", "---\nname: left\ndescription: left\n---\nbody\n")
	writeSkill(t, right, "right", "---\nname: right\ndescription: right\n---\nbody\n")
	registries := make([]*Registry, 2)
	for i, dir := range []string{left, right} {
		registries[i], _ = DiscoverDir(dir)
	}
	var wg sync.WaitGroup
	for i, dir := range []string{left, right} {
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			for range 20 {
				fresh, errs := DiscoverDir(dir)
				if len(errs) != 0 {
					t.Errorf("discover %s: %v", dir, errs)
					return
				}
				registries[i].Replace(fresh)
				_ = registries[i].Prompt()
				_, _ = registries[i].Get([]string{"left", "right"}[i])
			}
		}(i, dir)
	}
	wg.Wait()
	if registries[0].Len() != 1 || registries[1].Len() != 1 {
		t.Fatalf("concurrent refresh lost registry contents: %d, %d", registries[0].Len(), registries[1].Len())
	}
}

func TestReplaceSwapsBetweenNonEmptySets(t *testing.T) {
	a, _ := DiscoverFS(fstest.MapFS{
		"one/SKILL.md": {Data: []byte("---\nname: one\ndescription: one\n---\nbody\n")},
		"two/SKILL.md": {Data: []byte("---\nname: two\ndescription: two\n---\nbody\n")},
	})
	b, _ := DiscoverFS(fstest.MapFS{
		"three/SKILL.md": {Data: []byte("---\nname: three\ndescription: three\n---\nbody\n")},
	})
	a.Replace(b)
	if a.Len() != 1 {
		t.Fatalf("len = %d, want 1 (replace, not merge)", a.Len())
	}
	if _, ok := a.Get("one"); ok {
		t.Error("a replaced name must not survive in byName")
	}
	if got := a.ModelNames(); len(got) != 1 || got[0] != "three" {
		t.Errorf("ModelNames = %v, want [three]", got)
	}
}

// Outside a git repo the project root is cwd, so an ancestor's .skills belongs
// to no project. It used to be classified global and load with no approval at
// all, which bypassed the gate entirely for any unpacked (non-git) tree.
func TestAncestorSkillsOutsideGitRepoAreNotLoaded(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	writeSkill(t, filepath.Join(base, ".skills"), "planted",
		"---\nname: planted\ndescription: planted\n---\nbody\n")
	cwd := filepath.Join(base, "sub")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	r, _ := Discover(cwd)
	if _, ok := r.Get("planted"); ok {
		t.Error("an ancestor's skills must not load for a project that does not contain them")
	}
	p, err := Pending(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Errorf("they are out of scope, not withheld: %+v", p)
	}
}

// Inside a git repo the walk still reaches the repo root, which is the whole
// point of scanning ancestors.
func TestAncestorSkillsInsideGitRepoAreProjectLocal(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(root, ".skills"), "repo",
		"---\nname: repo\ndescription: repo\n---\nbody\n")
	cwd := filepath.Join(root, "pkg", "sub")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	p, err := Pending(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || len(p.Names) != 1 || p.Names[0] != "repo" {
		t.Fatalf("repo-root skills must be withheld and reported: %+v", p)
	}
	if err := ApproveProject(cwd); err != nil {
		t.Fatal(err)
	}
	r, _ := Discover(cwd)
	if s, ok := r.Get("repo"); !ok || !s.ProjectLocal {
		t.Errorf("repo-root skill not loaded as project-local: %+v", s)
	}
}

// A dotfiles repo rooted at $HOME must not turn the user's own global skills
// into project skills that they are asked to approve.
func TestGlobalSkillsAreNeverProjectLocal(t *testing.T) {
	isolate(t)
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "mine",
		"---\nname: mine\ndescription: mine\n---\nbody\n")
	writeSkill(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "aigem", "skills"), "alsomine",
		"---\nname: alsomine\ndescription: alsomine\n---\nbody\n")

	p, err := Pending(home)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatalf("the user's own skills must not need project approval: %+v", p)
	}
	r, _ := Discover(home)
	for _, name := range []string{"mine", "alsomine"} {
		s, ok := r.Get(name)
		if !ok {
			t.Fatalf("global skill %q was withheld", name)
		}
		if s.ProjectLocal {
			t.Errorf("global skill %q was marked project-local", name)
		}
	}
}

// An unreadable definition used to fail the fingerprint, which withheld every
// project skill AND suppressed the prompt explaining why: silence again.
func TestUnreadableDefinitionStillPrompts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	isolate(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".skills")
	writeSkill(t, root, "good", "---\nname: good\ndescription: good\n---\nbody\n")
	writeSkill(t, root, "bad", "---\nname: bad\ndescription: bad\n---\nbody\n")
	bad := filepath.Join(root, "bad", "SKILL.md")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bad, 0o644) })

	p, err := Pending(cwd)
	if err != nil {
		t.Fatalf("an unreadable definition must not fail evaluation: %v", err)
	}
	if p == nil {
		t.Fatal("the readable skills are still withheld, so the user must still be asked")
		return
	}
	if len(p.Names) != 2 {
		t.Errorf("names = %v, want both the readable and the unreadable one", p.Names)
	}
	if err := ApproveProject(cwd); err != nil {
		t.Fatalf("approval must still be possible: %v", err)
	}
	r, errs := Discover(cwd)
	if _, ok := r.Get("good"); !ok {
		t.Errorf("the readable skill should load after approval: %d skills, errs=%v", r.Len(), errs)
	}

	// Making it readable changes the definitions, so the approval is invalidated.
	if err := os.Chmod(bad, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err = Pending(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || !p.Invalidated {
		t.Errorf("a newly readable definition must re-open the decision: %+v", p)
	}
}

func TestUnreadableRootIsNamedAsADirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	isolate(t)
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".skills")
	writeSkill(t, root, "one", "---\nname: one\ndescription: one\n---\nbody\n")
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o755) })

	p, err := Pending(cwd)
	if err != nil {
		t.Fatalf("an unreadable root must not fail evaluation: %v", err)
	}
	if p == nil || len(p.Names) != 1 {
		t.Fatalf("pending = %+v, want the unreadable root reported", p)
	}
	// The label must name the directory, not resolve to "." the way taking the
	// parent of a SKILL.md path would.
	if got := p.Names[0]; got != ".skills/* (unreadable directory)" {
		t.Errorf("name = %q, want the directory itself", got)
	}
}

func TestOutOfScopeAncestorsAreReported(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	ancestor := filepath.Join(base, ".skills")
	writeSkill(t, ancestor, "planted", "---\nname: planted\ndescription: planted\n---\nbody\n")
	cwd := filepath.Join(base, "sub")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	got := OutOfScopeAncestors(cwd)
	var found bool
	for _, d := range got {
		if d == ancestor {
			found = true
		}
	}
	if !found {
		t.Errorf("OutOfScopeAncestors(%q) = %v, want it to name %q", cwd, got, ancestor)
	}
	// The user's own global dir is always loaded, so it is not out of scope.
	home := os.Getenv("HOME")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "mine",
		"---\nname: mine\ndescription: mine\n---\nbody\n")
	for _, d := range OutOfScopeAncestors(cwd) {
		if d == filepath.Join(home, ".claude", "skills") {
			t.Error("~/.claude/skills is in scope and must not be reported")
		}
	}

	// Inside a repo the ancestor is part of the project, so nothing is dropped.
	if err := os.MkdirAll(filepath.Join(base, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range OutOfScopeAncestors(cwd) {
		if d == ancestor {
			t.Error("a repo-root skill dir is in scope and must not be reported")
		}
	}
}
