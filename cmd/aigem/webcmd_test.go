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
	// The command reaches config.StateDir before web.New refuses, and without
	// this the run creates $HOME/.local/state/aigem on the machine running it.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	err := runWebCommand([]string{"--addr", "0.0.0.0:0"})
	if err == nil {
		t.Fatal("a non-loopback address was accepted")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error = %v, want it to explain the loopback rule", err)
	}
}

// The wiring for the flag that lifts the loopback rule. A malformed origin is
// the one way to reach web.New with origins set and still get an answer without
// serving - and the error can only come from there, so it proves the flag is
// registered, collected and passed through.
func TestWebCommandRefusesAMalformedOrigin(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	err := runWebCommand([]string{"--addr", "127.0.0.1:0", "--origin", "aigem.example.ts.net"})
	if err == nil {
		t.Fatal("an origin with no scheme was accepted")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error = %v, want it to say what is wrong", err)
	}
}

// Repeatable, because a daemon can be reached under a tailnet name and a LAN
// one at the same time.
func TestOriginListCollectsEveryFlag(t *testing.T) {
	var got originList
	for _, v := range []string{"https://a.test", "https://b.test"} {
		if err := got.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0] != "https://a.test" || got[1] != "https://b.test" {
		t.Errorf("originList = %v, want both in order", got)
	}
	if want := "https://a.test,https://b.test"; got.String() != want {
		t.Errorf("String() = %q, want %q", got.String(), want)
	}
}
