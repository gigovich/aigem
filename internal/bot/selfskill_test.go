package bot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/skill"
)

func TestSaveSkillRoundTrip(t *testing.T) {
	dir := t.TempDir()
	save := NewSaveSkillTool(dir)
	if save.Name() != "save_skill" || save.NeedsConfirm() {
		t.Fatalf("name=%q needsConfirm=%v", save.Name(), save.NeedsConfirm())
	}
	_, err := save.Run(context.Background(), json.RawMessage(
		`{"name":"Deploy Steps","description":"How to ship a release","body":"1. tag\n2. push"}`))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// The written skill is discoverable with the right metadata and body.
	reg, errs := skill.DiscoverDir(dir)
	if len(errs) != 0 {
		t.Fatalf("discover errors: %v", errs)
	}
	s, ok := reg.Get("Deploy Steps")
	if !ok {
		t.Fatal("saved skill not discovered")
	}
	if s.Description != "How to ship a release" || !strings.Contains(s.Body(), "2. push") {
		t.Fatalf("round-trip mismatch: desc=%q body=%q", s.Description, s.Body())
	}
}

func TestDeleteSkill(t *testing.T) {
	dir := t.TempDir()
	_, _ = NewSaveSkillTool(dir).Run(context.Background(),
		json.RawMessage(`{"name":"temp","description":"d","body":"b"}`))
	del := NewDeleteSkillTool(dir)
	if del.Name() != "delete_skill" || del.NeedsConfirm() {
		t.Fatalf("name=%q needsConfirm=%v", del.Name(), del.NeedsConfirm())
	}
	if _, err := del.Run(context.Background(), json.RawMessage(`{"name":"temp"}`)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	reg, _ := skill.DiscoverDir(dir)
	if reg.Len() != 0 {
		t.Fatalf("skill not removed, registry has %d", reg.Len())
	}
	if _, err := del.Run(context.Background(), json.RawMessage(`{"name":"temp"}`)); err == nil {
		t.Fatal("deleting a missing skill should error")
	}
}

func TestSaveSkillValidation(t *testing.T) {
	save := NewSaveSkillTool(t.TempDir())
	for _, args := range []string{
		`{"name":"x","description":"d"}`,              // no body
		`{"name":"x","body":"b"}`,                     // no description
		`{"description":"d","body":"b"}`,              // no name
		`{"name":"   ","description":"d","body":"b"}`, // blank name -> empty slug
	} {
		if _, err := save.Run(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("save(%s) should error", args)
		}
	}
}

func TestSaveSkillSlugCollision(t *testing.T) {
	dir := t.TempDir()
	save := NewSaveSkillTool(dir)
	run := func(args string) error {
		_, err := save.Run(context.Background(), json.RawMessage(args))
		return err
	}
	if err := run(`{"name":"Deploy Steps","description":"first","body":"body one"}`); err != nil {
		t.Fatal(err)
	}
	// A different name mapping to the same slug must error, not overwrite.
	if err := run(`{"name":"deploy steps","description":"second","body":"body two"}`); err == nil {
		t.Fatal("expected a collision error for a distinct name mapping to the same slug")
	}
	// The original survives.
	reg, _ := skill.DiscoverDir(dir)
	s, ok := reg.Get("Deploy Steps")
	if !ok || s.Description != "first" || !strings.Contains(s.Body(), "body one") {
		t.Fatalf("original skill damaged by collision attempt: ok=%v %+v", ok, s)
	}
	// Same name still overwrites (legitimate update).
	if err := run(`{"name":"Deploy Steps","description":"updated","body":"body three"}`); err != nil {
		t.Fatalf("same-name update should succeed: %v", err)
	}
}

func TestSaveSkillRejectsReservedNames(t *testing.T) {
	dir := t.TempDir()
	save := NewSaveSkillTool(dir)
	for _, name := range []string{"scheduling", "Memory Mechanics", "long-deliverables"} {
		args := `{"name":"` + name + `","description":"d","body":"b"}`
		if _, err := save.Run(context.Background(), json.RawMessage(args)); err == nil ||
			!strings.Contains(err.Error(), "reserved") {
			t.Errorf("save(%q) should error as reserved, got %v", name, err)
		}
	}
	reg, _ := skill.DiscoverDir(dir)
	if reg.Len() != 0 {
		t.Fatalf("reserved-name save must write nothing, registry has %d", reg.Len())
	}
}

func TestDeleteSkillRejectsBuiltins(t *testing.T) {
	del := NewDeleteSkillTool(t.TempDir())
	_, err := del.Run(context.Background(), json.RawMessage(`{"name":"long-deliverables"}`))
	if err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("deleting a built-in skill should error, got %v", err)
	}
}

func TestDeleteSkillRemovesLegacyReservedLeftover(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, "scheduling")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: scheduling\ndescription: legacy\n---\nlegacy body"
	if err := os.WriteFile(filepath.Join(leftover, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	del := NewDeleteSkillTool(dir)
	if _, err := del.Run(context.Background(), json.RawMessage(`{"name":"scheduling"}`)); err != nil {
		t.Fatalf("deleting an on-disk leftover under a reserved slug must succeed: %v", err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Error("leftover dir not removed")
	}
	// With nothing on disk the reserved name is refused again.
	if _, err := del.Run(context.Background(), json.RawMessage(`{"name":"scheduling"}`)); err == nil ||
		!strings.Contains(err.Error(), "built-in") {
		t.Fatalf("expected built-in refusal after cleanup, got %v", err)
	}
}
