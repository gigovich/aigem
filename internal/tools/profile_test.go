package tools

import "testing"

func TestCapabilityProfileDefaults(t *testing.T) {
	p, err := ResolveCapabilityProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != DefaultCapabilityProfile {
		t.Fatalf("default profile = %q, want %q", p.Name, DefaultCapabilityProfile)
	}
	if p.Allows("bash") {
		t.Fatal("default profile must not expose bash")
	}
	if !p.Allows("write_file") || !p.Allows("edit_file") {
		t.Fatalf("default profile should allow file editing tools: %v", p.Allow)
	}
}

func TestCapabilityShellProfiles(t *testing.T) {
	shell, err := ResolveCapabilityProfile("shell")
	if err != nil {
		t.Fatal(err)
	}
	if !shell.Allows("bash") || !shell.AutoApproveBash || shell.AutoApproveDestructiveBash {
		t.Fatalf("unexpected shell profile: %+v", shell)
	}
	dangerous, err := ResolveCapabilityProfile("dangerous-shell")
	if err != nil {
		t.Fatal(err)
	}
	if !dangerous.Allows("bash") || !dangerous.AutoApproveBash || !dangerous.AutoApproveDestructiveBash {
		t.Fatalf("unexpected dangerous-shell profile: %+v", dangerous)
	}
}

func TestResolveCapabilityProfileRejectsUnknown(t *testing.T) {
	if _, err := ResolveCapabilityProfile("root"); err == nil {
		t.Fatal("expected unknown profile error")
	}
}
