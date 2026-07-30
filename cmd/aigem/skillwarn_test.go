package main

import (
	"strings"
	"testing"

	"github.com/gigovich/aigem/internal/skill"
)

func TestPendingSkillsWarning(t *testing.T) {
	if got := pendingSkillsWarning(nil); got != "" {
		t.Errorf("nothing pending must produce no warning, got %q", got)
	}

	got := pendingSkillsWarning(&skill.PendingSkills{Dir: "/p", Names: []string{"gitea", "deploy"}})
	for _, want := range []string{"warning:", "/p", "untrusted", "gitea", "deploy", "--trust-project-skills"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("the warning must be a single line: %q", got)
	}

	changed := pendingSkillsWarning(&skill.PendingSkills{Dir: "/p", Names: []string{"gitea"}, Invalidated: true})
	if !strings.Contains(changed, "changed since you approved them") {
		t.Errorf("an invalidated approval must say so, got %q", changed)
	}

	// A definition that could not be read is still named, by its directory, so
	// the warning never renders an empty list.
	unreadable := pendingSkillsWarning(&skill.PendingSkills{Dir: "/p", Names: []string{"bad (unreadable)"}})
	if !strings.Contains(unreadable, "bad (unreadable)") {
		t.Errorf("an unreadable definition must still be named, got %q", unreadable)
	}
}
