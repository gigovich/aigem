package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHandoffDefaultsToCoordinationChannel(t *testing.T) {
	fp := &fakePoster{}
	tool := NewHandoffTool(fp, fakeResolver{ids: map[string]string{defaultHandoffChannel: "id-tasks"}})
	if tool.Name() != "handoff" || tool.NeedsConfirm() {
		t.Fatalf("name=%q needsConfirm=%v", tool.Name(), tool.NeedsConfirm())
	}
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"to":"jane","summary":"run QA on #4"}`)); err != nil {
		t.Fatal(err)
	}
	if fp.channel != "id-tasks" {
		t.Fatalf("posted to %q, want id-tasks (default channel)", fp.channel)
	}
	if !strings.HasPrefix(fp.text, "@jane") {
		t.Fatalf("handoff must @mention the teammate, got %q", fp.text)
	}
	if !strings.Contains(fp.text, "run QA on #4") {
		t.Fatalf("handoff should carry the summary, got %q", fp.text)
	}
	if fp.thread != "" {
		t.Fatalf("no thread expected, got %q", fp.thread)
	}
}

func TestHandoffNamedChannelTicketAndThread(t *testing.T) {
	fp := &fakePoster{}
	tool := NewHandoffTool(fp, fakeResolver{ids: map[string]string{"Bootcamp": "id-bc"}})
	if _, err := tool.Run(context.Background(), json.RawMessage(
		`{"to":"@@kate","summary":"review design","ticket":"AIGEM-4","channel":"Bootcamp","thread":"root1"}`,
	)); err != nil {
		t.Fatal(err)
	}
	if fp.channel != "id-bc" || fp.thread != "root1" {
		t.Fatalf("posted to %q thread %q, want id-bc root1", fp.channel, fp.thread)
	}
	// Any number of leading @ on the target must collapse to a single mention.
	if !strings.HasPrefix(fp.text, "@kate") || strings.Contains(fp.text, "@@") {
		t.Fatalf("target mention malformed: %q", fp.text)
	}
	if !strings.Contains(fp.text, "AIGEM-4") {
		t.Fatalf("ticket should appear in the message, got %q", fp.text)
	}
}

func TestHandoffRequiresToAndSummary(t *testing.T) {
	fp := &fakePoster{}
	tool := NewHandoffTool(fp, fakeResolver{ids: map[string]string{defaultHandoffChannel: "id-tasks"}})
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"summary":"x"}`)); err == nil {
		t.Error("missing to should error")
	}
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"to":"jane"}`)); err == nil {
		t.Error("missing summary should error")
	}
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"to":"  ","summary":"x"}`)); err == nil {
		t.Error("blank to should error")
	}
	if fp.calls != 0 {
		t.Fatalf("no handoff should be posted on validation failure, got %d", fp.calls)
	}
}

func TestHandoffUnknownChannelErrors(t *testing.T) {
	fp := &fakePoster{}
	tool := NewHandoffTool(fp, fakeResolver{ids: map[string]string{defaultHandoffChannel: "id-tasks"}})
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"to":"jane","summary":"x","channel":"nope"}`)); err == nil {
		t.Error("unknown channel should error")
	}
	if fp.calls != 0 {
		t.Fatalf("no handoff should be posted when the channel does not resolve, got %d", fp.calls)
	}
}
