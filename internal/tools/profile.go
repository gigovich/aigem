package tools

import (
	"fmt"
	"strings"
)

// CapabilityProfile names a tool/capability envelope for unattended front-ends.
// Profiles restrict which tools are exposed at all and how bash may be
// auto-approved when there is no human confirmation prompt.
type CapabilityProfile struct {
	Name                       string
	Description                string
	Allow                      []string
	AutoApproveBash            bool
	AutoApproveDestructiveBash bool
}

var baseReadTools = []string{"read_file", "list_dir", "grep", "fuzzy_find", "web_search", "open_url", "browser_action"}
var baseWriteTools = []string{"write_file", "edit_file"}

// botTools are the tools only the fleet has. Every profile allows all of them:
// a profile bounds what a bot may do to the machine, and talking to its own
// threads is not that.
//
// It must stay in step with roles.full - a name here that no tool answers to is
// harmless, but a tool missing from here is silently absent from every bot,
// however plainly the role allows it. "read_chat" sat here for a release after
// the tool was renamed read_threads, and no bot could search its own threads
// for the whole of it. TestEveryRoleToolIsInTheBotProfile is what catches that.
var botTools = []string{
	"memory", "schedule", "post_message", "handoff", "read_threads", "team_status",
	"save_skill", "delete_skill", "skill", "todo_write",
}

// CapabilityProfiles are ordered from least to most permissive. The default for
// non-interactive and bot use is workspace-write: filesystem edits are possible
// only through the audited file tools, while arbitrary shell is absent.
var CapabilityProfiles = []CapabilityProfile{
	{
		Name:        "read-only",
		Description: "read/search tools only; no writes and no shell",
		Allow:       appendSlices(baseReadTools, botTools),
	},
	{
		Name:        "workspace-write",
		Description: "read/search plus write_file/edit_file; no shell (safe unattended default)",
		Allow:       appendSlices(baseReadTools, baseWriteTools, botTools),
	},
	{
		Name:            "shell",
		Description:     "workspace-write plus bash; unattended auto-approval denies destructive bash",
		Allow:           appendSlices(baseReadTools, baseWriteTools, []string{"bash"}, botTools),
		AutoApproveBash: true,
	},
	{
		Name:                       "dangerous-shell",
		Description:                "shell with destructive bash auto-approval; catastrophic patterns remain hard-denied",
		Allow:                      appendSlices(baseReadTools, baseWriteTools, []string{"bash"}, botTools),
		AutoApproveBash:            true,
		AutoApproveDestructiveBash: true,
	},
}

// DefaultCapabilityProfile is the safe profile for unattended/non-interactive use.
const DefaultCapabilityProfile = "workspace-write"

func appendSlices(parts ...[]string) []string {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]string, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ResolveCapabilityProfile returns the named profile, or the default when name
// is empty. Names are stable CLI/config values.
func ResolveCapabilityProfile(name string) (CapabilityProfile, error) {
	if name == "" {
		name = DefaultCapabilityProfile
	}
	for _, p := range CapabilityProfiles {
		if p.Name == name {
			return p, nil
		}
	}
	return CapabilityProfile{}, fmt.Errorf("unknown capability profile %q (valid: %s)", name, strings.Join(CapabilityProfileNames(), ", "))
}

// CapabilityProfileNames returns the valid profile names in display order.
func CapabilityProfileNames() []string {
	out := make([]string, len(CapabilityProfiles))
	for i, p := range CapabilityProfiles {
		out[i] = p.Name
	}
	return out
}

func (p CapabilityProfile) Allows(name string) bool {
	// Compare the bare name: a subagent's call arrives as "<agent> › bash".
	bare := BaseToolName(name)
	for _, allowed := range p.Allow {
		if allowed == bare {
			return true
		}
	}
	return false
}
