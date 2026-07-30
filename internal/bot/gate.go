package bot

import (
	"encoding/json"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/tools"
)

// AllowGate returns a ConfirmFunc that approves a tool call only when the tool is in the
// bot's capability profile. This is the second of two enforcement layers: the runtime first
// builds the agent's registry with the role/profile intersection, so non-allowed tools are
// not even callable; the agent calls a ConfirmFunc only for confirm-gated tools (write_file,
// edit_file, bash), and this gate denies any of those not in the profile. The runtime has no
// human to prompt, so a denied tool simply returns false and the model sees the refusal.
// Subagent attribution prefixes ("agent › tool") are stripped before matching.
func AllowGate(profile tools.CapabilityProfile) agent.ConfirmFunc {
	set := make(map[string]bool, len(profile.Allow))
	for _, name := range profile.Allow {
		set[name] = true
	}
	return func(toolName string, args json.RawMessage) bool {
		name := tools.BaseToolName(toolName)
		if !set[name] {
			return false
		}
		if name == "bash" {
			if !profile.AutoApproveBash {
				return false
			}
			if tools.IsDestructive(name, args) && !profile.AutoApproveDestructiveBash {
				return false
			}
		}
		return true
	}
}
