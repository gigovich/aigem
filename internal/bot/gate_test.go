package bot

import (
	"encoding/json"
	"testing"

	"github.com/gigovich/aigem/internal/tools"
)

func TestAllowGate(t *testing.T) {
	profile, err := tools.ResolveCapabilityProfile("workspace-write")
	if err != nil {
		t.Fatal(err)
	}
	gate := AllowGate(profile)
	if !gate("read_file", json.RawMessage(`{}`)) {
		t.Error("read_file should be allowed")
	}
	if gate("bash", json.RawMessage(`{"cmd":"go test ./..."}`)) {
		t.Error("bash should be denied by the default workspace-write profile")
	}
	// Attribution prefixes added by subagents must not defeat the gate.
	if !gate("scout › read_file", json.RawMessage(`{}`)) {
		t.Error("attributed allowed tool should still be allowed")
	}
}

func TestAllowGateShellProfile(t *testing.T) {
	profile, err := tools.ResolveCapabilityProfile("shell")
	if err != nil {
		t.Fatal(err)
	}
	gate := AllowGate(profile)
	if !gate("bash", json.RawMessage(`{"cmd":"go test ./..."}`)) {
		t.Error("shell profile should allow non-destructive bash")
	}
	if gate("bash", json.RawMessage(`{"cmd":"rm generated.txt"}`)) {
		t.Error("shell profile should deny destructive bash")
	}
}

func TestAllowGateDangerousShellProfile(t *testing.T) {
	profile, err := tools.ResolveCapabilityProfile("dangerous-shell")
	if err != nil {
		t.Fatal(err)
	}
	gate := AllowGate(profile)
	if !gate("bash", json.RawMessage(`{"cmd":"rm generated.txt"}`)) {
		t.Error("dangerous-shell profile should allow destructive bash")
	}
}

func TestAllowGateEmpty(t *testing.T) {
	gate := AllowGate(tools.CapabilityProfile{})
	if gate("read_file", json.RawMessage(`{}`)) {
		t.Error("empty allowlist must deny every tool")
	}
}
