package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// --sign-out is the remediation for a leaked token, so it has to be the thing
// that failed when it fails, rather than a flag the command served past.
func TestSignOutWithNoStateDirectoryIsRefused(t *testing.T) {
	// A state "directory" that is really a file: StateDir cannot create anything
	// under it, so there is no sessions file to forget.
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blocked)

	// On a deadline: a command that does not refuse here goes on to serve, and
	// an inline call would report that as the package timing out rather than as
	// this test failing.
	done := make(chan error, 1)
	go func() { done <- runWebCommand([]string{"--addr", "127.0.0.1:0", "--sign-out"}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("--sign-out reported success with nothing to forget")
		}
		if !strings.Contains(err.Error(), "sign-out") {
			t.Errorf("error = %v, want it to name the flag that could not be honoured", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("--sign-out served on without forgetting anything")
	}
}

// The sessions really are gone afterwards, and the daemon still starts.
func TestSignOutForgetsTheSessionsFile(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	file := filepath.Join(state, "aigem", "web-cookies.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"sessions":{"a":"2099-01-01T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// A malformed origin stops the command right after the forget, so the test
	// does not have to serve anything to observe it.
	if err := runWebCommand([]string{"--addr", "127.0.0.1:0", "--sign-out", "--origin", "nonsense"}); err == nil {
		t.Fatal("the malformed origin was accepted, so this test proves nothing")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the sessions file survived --sign-out: %v", err)
	}
}
