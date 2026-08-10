package auth

import (
	"strings"
	"testing"
)

// The paste path is what makes the ChatGPT flow usable from a phone: the
// provider redirects that browser to its own localhost, which is not this
// machine. It has to accept both what the user can actually copy - the whole
// redirected URL - and a bare code, and it must mark the result as pasted,
// because that is what the CSRF rule keys off.
func TestPasteAcceptsURLAndBareCode(t *testing.T) {
	for _, c := range []struct {
		name, in, code, state string
	}{
		{"full url", "http://localhost:1455/auth/callback?code=abc&state=xyz", "abc", "xyz"},
		{"url without state", "http://localhost:1455/auth/callback?code=abc", "abc", ""},
		{"bare code", "  abc  ", "abc", ""},
	} {
		cs := &callbackServer{results: make(chan callbackResult, 1)}
		if !cs.paste(c.in) {
			t.Fatalf("%s: paste was refused", c.name)
		}
		got := <-cs.results
		if got.code != c.code || got.state != c.state {
			t.Errorf("%s: code=%q state=%q, want %q/%q", c.name, got.code, got.state, c.code, c.state)
		}
		if !got.viaPaste {
			t.Errorf("%s: the result was not marked as pasted", c.name)
		}
	}
}

func TestPasteRefusesNothing(t *testing.T) {
	cs := &callbackServer{results: make(chan callbackResult, 1)}
	if cs.paste("   ") {
		t.Fatal("an empty paste was accepted")
	}
}

// Only one answer settles a login. A second must not overwrite the first, or a
// pasted code could displace one that already arrived over the callback.
func TestPasteDoesNotOverwriteAnAnswer(t *testing.T) {
	cs := &callbackServer{results: make(chan callbackResult, 1)}
	if !cs.paste("first") {
		t.Fatal("the first paste was refused")
	}
	if cs.paste("second") {
		t.Fatal("a second answer was accepted")
	}
	if got := <-cs.results; got.code != "first" {
		t.Fatalf("code = %q, want the first answer", got.code)
	}
}

// A device flow has no callback, so there is nowhere for a pasted code to go.
// Accepting one would mean taking an authorization code from a different login.
func TestDeviceFlowRefusesPaste(t *testing.T) {
	f := &Flow{Provider: "xai", state: FlowPending}
	err := f.Paste("http://localhost:1455/auth/callback?code=abc")
	if err == nil || !strings.Contains(err.Error(), "does not take") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// Cancelling leaves the credential store alone and reports the flow as failed
// rather than pending forever.
func TestCancelMarksFailed(t *testing.T) {
	f := &Flow{Provider: "xai", state: FlowPending}
	f.Cancel()
	if state, err := f.Status(); state != FlowFailed || err == nil {
		t.Fatalf("state=%q err=%v, want failed with a reason", state, err)
	}
}
