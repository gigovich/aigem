package agent

import (
	"testing"

	"github.com/gigovich/aigem/internal/tools"
)

// TestEveryCapabilityProfileAllowsSkillAndTodoTools guards against a repeat of
// the read_chat/read_threads incident: a tool renamed in this package silently
// falling out of every capability profile's allowlist in internal/tools. -p
// depends on both tools in every profile, and dropping either produces no
// compile error and no other test failure - only planning or skills quietly
// stop working in unattended runs. It compares against the constants, not the
// string literals, so a rename of either constant fails this test too.
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
