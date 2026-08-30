package agent

import (
	"testing"

	"github.com/gigovich/aigem/internal/tools"
)

// TestEveryCapabilityProfileAllowsSkillAndTodoTools guards against a repeat of
// the read_chat/read_threads incident: a tool renamed in this package silently
// falling out of every capability profile's allowlist in internal/tools, with
// no compile error and no other test failure to show for it. -p does not depend
// on those two entries as the code stands - cmd/aigem/main.go registers the
// skill and todo tools after it applies Subset(profile.Allow), and Subset copies
// a name only if the registry already holds it - so this is forward protection
// against a change in registration order. It compares against the constants,
// not the string literals, so a rename of either constant fails this test too.
func TestEveryCapabilityProfileAllowsSkillAndTodoTools(t *testing.T) {
	for _, p := range tools.CapabilityProfiles {
		if !p.Allows(SkillToolName) {
			t.Errorf("capability profile %q does not allow %q", p.Name, SkillToolName)
		}
		if !p.Allows(TodoToolName) {
			t.Errorf("capability profile %q does not allow %q", p.Name, TodoToolName)
		}
	}
}
