package bot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var wantSystemSkills = []string{"long-deliverables", "memory-hygiene", "memory-mechanics", "scheduling"}

func TestSystemSkillsDiscovered(t *testing.T) {
	reg, errs := SystemSkills()
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got := len(reg.List()); got != len(wantSystemSkills) {
		t.Fatalf("got %d skills, want %d", got, len(wantSystemSkills))
	}
	for _, name := range wantSystemSkills {
		s, ok := reg.Get(name)
		if !ok {
			t.Errorf("missing system skill %q", name)
			continue
		}
		if !s.Builtin {
			t.Errorf("%s: not marked Builtin", name)
		}
		if !s.ModelInvocable() {
			t.Errorf("%s: not model-invocable", name)
		}
		if s.Description == "" {
			t.Errorf("%s: empty description", name)
		}
	}
	if reg.Prompt() == "" {
		t.Error("system skills catalog Prompt() should be non-empty")
	}
}

// Embedded skills have no on-disk Dir, so dynamic injection and ${CLAUDE_SKILL_DIR}
// would run a shell in the wrong place; their bodies must stay static.
func TestSystemSkillBodiesAreStatic(t *testing.T) {
	reg, _ := SystemSkills()
	for _, s := range reg.List() {
		for _, banned := range []string{"!`", "```!", "${CLAUDE_SKILL_DIR}"} {
			if strings.Contains(s.Body(), banned) {
				t.Errorf("%s: body contains %q", s.Name, banned)
			}
		}
	}
}

func TestSystemSkillBodiesCarryMovedContent(t *testing.T) {
	reg, _ := SystemSkills()
	want := map[string][]string{
		"scheduling": {
			"minute hour day-of-month month day-of-week", "FRESH agent", "@username",
			"root post id", "set:", "remove:", "list:",
		},
		"long-deliverables": {"16k", "write_file", "wiki", "short summary"},
		"memory-mechanics": {
			"save:", "read:", "delete:", "list:", "archive:", "restore:", "audit:", "inspect:", "overwrites it",
		},
		"memory-hygiene": {
			"audit", "PROTECTED", "schedule list", "archive", "inspect", "memory-review-log", "Never post",
		},
	}
	for name, needles := range want {
		s, ok := reg.Get(name)
		if !ok {
			t.Errorf("missing system skill %q", name)
			continue
		}
		for _, n := range needles {
			if !strings.Contains(s.Body(), n) {
				t.Errorf("%s: body missing %q", name, n)
			}
		}
	}
}

func TestDiscoverBotSkillsShadowsSelfSkills(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "scheduling")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: scheduling\ndescription: rogue override\n---\nrogue body"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "---\nname: mine\ndescription: self-authored\n---\nmy body"
	if err := os.WriteFile(filepath.Join(dir, "mine", "SKILL.md"), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, errs := DiscoverBotSkills(dir)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "shadowed") {
		t.Fatalf("shadowed self-skill must surface as one error, got %v", errs)
	}
	s, ok := reg.Get("scheduling")
	if !ok {
		t.Fatal("built-in scheduling skill missing")
	}
	if !s.Builtin || strings.Contains(s.Body(), "rogue") {
		t.Error("built-in scheduling skill must shadow the self-authored one")
	}
	if _, ok := reg.Get("mine"); !ok {
		t.Error("self-authored skill with a free name should be discovered")
	}
}

// A pre-guard self-skill whose name is a case variant of a builtin would otherwise appear
// twice in the catalog; discovery must exclude it and report it.
func TestDiscoverBotSkillsExcludesReservedSlugVariants(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "scheduling")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: Scheduling\ndescription: legacy variant\n---\nlegacy body"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, errs := DiscoverBotSkills(dir)
	if len(errs) != 1 {
		t.Fatalf("expected one shadowing error, got %v", errs)
	}
	if _, ok := reg.Get("Scheduling"); ok {
		t.Error("case-variant of a reserved name must not appear beside the builtin")
	}
	if s, ok := reg.Get("scheduling"); !ok || !s.Builtin {
		t.Error("builtin must remain")
	}
}
