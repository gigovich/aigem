package auth

import (
	"strings"
	"testing"
)

func TestFlowCancelIsIdempotentAndTerminal(t *testing.T) {
	f := &Flow{Provider: "xai", URL: "https://example.test", Code: "secret", state: FlowPending}
	f.Cancel()
	f.Cancel()
	state, err := f.Status()
	if state != FlowFailed || err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("Status = %q, %v, want a cancelled failure", state, err)
	}
	if url, code, _ := f.Display(); url != "" || code != "" {
		t.Fatalf("terminal flow retained URL/code %q/%q", url, code)
	}
}

func TestDeviceFlowRefusesPastedCodes(t *testing.T) {
	f := &Flow{Provider: "xai", state: FlowPending}
	if err := f.Paste("code"); err == nil {
		t.Fatal("a device flow accepted a pasted authorization code")
	}
}

func TestFlowPasteAcceptsRedirectURLAndBareCode(t *testing.T) {
	for _, tc := range []struct {
		in, code, state string
	}{
		{"http://localhost:1455/auth/callback?code=abc&state=xyz", "abc", "xyz"},
		{"  abc  ", "abc", ""},
	} {
		cb := &callbackServer{results: make(chan callbackResult, 1)}
		f := &Flow{Provider: "openai", AcceptsPaste: true, state: FlowPending, cb: cb}
		if err := f.Paste(tc.in); err != nil {
			t.Fatalf("Paste(%q): %v", tc.in, err)
		}
		got := <-cb.results
		if got.code != tc.code || got.state != tc.state || !got.viaPaste {
			t.Fatalf("Paste(%q) delivered %+v, want code/state %q/%q via paste", tc.in, got, tc.code, tc.state)
		}
	}
}

func TestFlowPasteDoesNotOverwriteAnAnswer(t *testing.T) {
	cb := &callbackServer{results: make(chan callbackResult, 1)}
	f := &Flow{Provider: "openai", AcceptsPaste: true, state: FlowPending, cb: cb}
	if err := f.Paste("first"); err != nil {
		t.Fatal(err)
	}
	if err := f.Paste("second"); err == nil {
		t.Fatal("a second authorization answer was accepted")
	}
	if got := <-cb.results; got.code != "first" {
		t.Fatalf("code = %q, want first", got.code)
	}
}
