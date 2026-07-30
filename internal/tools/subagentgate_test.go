package tools

import (
	"encoding/json"
	"testing"
)

// Subagent confirmations arrive decorated as "code-writer › bash" for display.
// Every policy check keyed to a tool name has to see the bare name, or a
// subagent's destructive command silently misses the rule that a main-agent
// command would hit - auto mode would approve `rm -rf` without asking.
func TestIsDestructiveSeesThroughSubagentAttribution(t *testing.T) {
	destructive := json.RawMessage(`{"cmd":"rm -rf /important"}`)
	safe := json.RawMessage(`{"cmd":"ls -la"}`)

	for _, name := range []string{
		"bash",
		"code-writer › bash",
		"simplifier › bash",
		"reviewer › bash",
	} {
		if !IsDestructive(name, destructive) {
			t.Errorf("IsDestructive(%q, rm -rf) = false; a destructive command escaped the gate", name)
		}
		if IsDestructive(name, safe) {
			t.Errorf("IsDestructive(%q, ls) = true; a safe command was misclassified", name)
		}
	}

	// Non-bash tools stay non-destructive, decorated or not.
	for _, name := range []string{"write_file", "code-writer › write_file"} {
		if IsDestructive(name, destructive) {
			t.Errorf("IsDestructive(%q) = true; only bash is classified destructive", name)
		}
	}
}

func TestBaseToolName(t *testing.T) {
	cases := map[string]string{
		"bash":               "bash",
		"code-writer › bash": "bash",
		"a › b › bash":       "bash",
		"  spaced › bash  ":  "bash",
		"write_file":         "write_file",
		"":                   "",
	}
	for in, want := range cases {
		if got := BaseToolName(in); got != want {
			t.Errorf("BaseToolName(%q) = %q, want %q", in, got, want)
		}
	}
}
