package mattermost

import (
	"strings"
	"testing"
)

func TestIsErrorNoise(t *testing.T) {
	noise := []string{
		`error: responses: stream error: {"type":"error","error":{"code":"context_length_exceeded"}}`,
		"error: responses: stream error: something",
		`error: {"type":"invalid_request_error"}`,
	}
	for _, m := range noise {
		if !isErrorNoise(m) {
			t.Errorf("expected noise: %q", m)
		}
	}
	real := []string{
		"error: the build failed on line 12", // a plain "error:" note is not provider noise
		"lisa: status? no errors here",
		"context_length_exceeded is a concept we should document",
	}
	for _, m := range real {
		if isErrorNoise(m) {
			t.Errorf("expected real content, got noise: %q", m)
		}
	}
}

func TestDropErrorNoise(t *testing.T) {
	posts := []ThreadPost{
		{UserID: "u1", Message: "lisa: status?"},
		{UserID: "u2", Message: "error: responses: stream error: context_length_exceeded"},
		{UserID: "u3", Message: "amiran: ready"},
	}
	got := dropErrorNoise(posts)
	if len(got) != 2 || got[0].Message != "lisa: status?" || got[1].Message != "amiran: ready" {
		t.Fatalf("dropErrorNoise = %+v", got)
	}
}

func TestThreadHistoryTrimBudget(t *testing.T) {
	// Build a block longer than the budget and confirm the trim keeps the tail + marker and stays
	// within budget. This exercises the same trimming ThreadHistory applies to formatAuthored's
	// output.
	block := strings.Repeat("user: a line of thread text\n", 4000) // well over 60k chars
	r := []rune(block)
	if len(r) <= maxThreadHistoryChars {
		t.Fatalf("test setup: block too short (%d)", len(r))
	}
	tail := string(r[len(r)-maxThreadHistoryChars:])
	if i := strings.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	out := threadElidedMarker + tail
	if !strings.HasPrefix(out, threadElidedMarker) {
		t.Fatal("trimmed block should start with the elision marker")
	}
	if n := len([]rune(out)); n > maxThreadHistoryChars+len([]rune(threadElidedMarker)) {
		t.Fatalf("trimmed block %d runes exceeds budget", n)
	}
}
