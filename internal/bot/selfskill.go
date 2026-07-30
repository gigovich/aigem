package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gigovich/aigem/internal/tools"
)

type skillMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// reservedSkillSlug reports whether slug collides with a built-in system skill, whose names
// are reserved so discovery never has to arbitrate between a builtin and a self-authored file.
func reservedSkillSlug(slug string) bool {
	for _, n := range SystemSkillNames() {
		if slugify(n) == slug {
			return true
		}
	}
	return false
}

// writeSkill saves a plain instruction skill as <skillsDir>/<slug>/SKILL.md. Only name and
// description frontmatter is written, so a self-authored skill can never request tools or a
// forked context the role does not already allow.
func writeSkill(skillsDir, name, description, body string) error {
	slug := slugify(name)
	if slug == "" {
		return fmt.Errorf("skill name %q is empty after normalization", name)
	}
	if reservedSkillSlug(slug) {
		return fmt.Errorf("skill name %q is reserved by a built-in skill; choose another name", name)
	}
	dir := filepath.Join(skillsDir, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	skillPath := filepath.Join(dir, "SKILL.md")
	if existing, rerr := os.ReadFile(skillPath); rerr == nil {
		if f := parseFact(existing); f.Name != "" && f.Name != name {
			return fmt.Errorf("skill name %q collides with existing skill %q (both map to %s); choose a more distinct name",
				name, f.Name, slug)
		}
	}
	fm, err := yaml.Marshal(skillMeta{Name: name, Description: description})
	if err != nil {
		return err
	}
	content := "---\n" + string(fm) + "---\n" + strings.TrimRight(body, "\n") + "\n"
	return os.WriteFile(skillPath, []byte(content), 0o644)
}

func deleteSkill(skillsDir, name string) error {
	slug := slugify(name)
	if slug == "" {
		return fmt.Errorf("skill name %q is empty after normalization", name)
	}
	dir := filepath.Join(skillsDir, slug)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			if reservedSkillSlug(slug) {
				return fmt.Errorf("%q is a built-in skill and cannot be deleted", name)
			}
			return fmt.Errorf("no skill named %q", name)
		}
		return err
	}
	// An on-disk dir under a reserved slug is a self-authored leftover from before the name
	// became reserved (discovery shadows it); deleting it is the migration escape hatch.
	return os.RemoveAll(dir)
}

type saveSkillTool struct{ dir string }

// NewSaveSkillTool lets the bot author a reusable skill. Not confirm-gated: the bot owns its
// skills.
func NewSaveSkillTool(skillsDir string) tools.Tool { return &saveSkillTool{dir: skillsDir} }

func (t *saveSkillTool) Name() string       { return "save_skill" }
func (t *saveSkillTool) NeedsConfirm() bool { return false }

func (t *saveSkillTool) Description() string {
	return "Capture a reusable procedure as a skill so future runs can apply it. Give it a short " +
		"name, a one-line description that says WHEN to use it, and a step-by-step body. Saving a " +
		"name that already exists replaces it. Skills you save appear in your skills catalog on " +
		"subsequent runs and are loaded with the skill tool."
}

func (t *saveSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"name":{"type":"string","description":"short skill name"},
			"description":{"type":"string","description":"one line: when to use this skill"},
			"body":{"type":"string","description":"the step-by-step instructions"}
		},
		"required":["name","description","body"]
	}`)
}

func (t *saveSkillTool) Run(_ context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(rawArgs, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Description) == "" ||
		strings.TrimSpace(a.Body) == "" {
		return "", fmt.Errorf("save_skill requires name, description, and body")
	}
	if err := writeSkill(t.dir, strings.TrimSpace(a.Name), strings.TrimSpace(a.Description), a.Body); err != nil {
		return "", err
	}
	return fmt.Sprintf("saved skill %q", strings.TrimSpace(a.Name)), nil
}

type deleteSkillTool struct{ dir string }

// NewDeleteSkillTool removes one of the bot's self-authored skills.
func NewDeleteSkillTool(skillsDir string) tools.Tool { return &deleteSkillTool{dir: skillsDir} }

func (t *deleteSkillTool) Name() string       { return "delete_skill" }
func (t *deleteSkillTool) NeedsConfirm() bool { return false }

func (t *deleteSkillTool) Description() string {
	return "Delete one of your saved skills by name when it is obsolete."
}

func (t *deleteSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"name":{"type":"string","description":"the skill to delete"}},
		"required":["name"]
	}`)
}

func (t *deleteSkillTool) Run(_ context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rawArgs, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Name) == "" {
		return "", fmt.Errorf("delete_skill requires name")
	}
	if err := deleteSkill(t.dir, a.Name); err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted skill %q", a.Name), nil
}
