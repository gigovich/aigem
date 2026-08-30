package main

import (
	"strings"
	"testing"
)

// The command returns before serving on each of these, so they are the paths a
// test can reach without binding anything.
func TestWebCommandRejectsBadInputWithUsage(t *testing.T) {
	err := runWebCommand([]string{"--bogus"})
	if err == nil {
		t.Fatal("an unknown flag was accepted")
	}
	if !strings.Contains(err.Error(), "not defined") {
		t.Errorf("error = %v, want it to name the flag problem", err)
	}
	// The flag package's own output is discarded so the message is printed once,
	// by the caller - which means the usage has to travel with the error.
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("error = %v, want the usage block attached", err)
	}
}

func TestWebCommandHelpIsNotAnError(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		if err := runWebCommand([]string{flag}); err != nil {
			t.Errorf("runWebCommand(%q) = %v, want nil", flag, err)
		}
	}
}

// The refusal is the daemon's only access control, so the command has to
// surface it rather than fall back to a loopback port and look like it worked.
func TestWebCommandRefusesANonLoopbackAddress(t *testing.T) {
	err := runWebCommand([]string{"--addr", "0.0.0.0:0"})
	if err == nil {
		t.Fatal("a non-loopback address was accepted")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error = %v, want it to explain the loopback rule", err)
	}
}
