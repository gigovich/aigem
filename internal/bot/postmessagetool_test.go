package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakePoster struct {
	channel, text, thread string
	calls                 int
}

func (f *fakePoster) Post(channel, text string) error {
	f.channel, f.text = channel, text
	f.calls++
	return nil
}

func (f *fakePoster) PostToThread(channelID, rootID, text string) error {
	f.channel, f.thread, f.text = channelID, rootID, text
	f.calls++
	return nil
}

type fakeResolver struct {
	ids     map[string]string
	members []string
}

func (f fakeResolver) ResolveChannel(_ context.Context, name string) (string, error) {
	if id, ok := f.ids[name]; ok {
		return id, nil
	}
	return "", errUnknownChannel{name}
}

func (f fakeResolver) MemberChannels(_ context.Context) ([]string, error) { return f.members, nil }

type errUnknownChannel struct{ name string }

func (e errUnknownChannel) Error() string { return "not a member of " + e.name }

func TestPostMessageNamedChannel(t *testing.T) {
	fp := &fakePoster{}
	tool := NewPostMessageTool(fp, fakeResolver{ids: map[string]string{"a": "id-a", "b": "id-b"}}, nil)
	if tool.Name() != "post_message" || tool.NeedsConfirm() {
		t.Fatalf("name=%q needsConfirm=%v", tool.Name(), tool.NeedsConfirm())
	}
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"channel":"b","text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if fp.channel != "id-b" || fp.text != "hi" {
		t.Fatalf("posted to %q %q, want id-b hi", fp.channel, fp.text)
	}
}

func TestPostMessageMissingChannelListsMembers(t *testing.T) {
	fp := &fakePoster{}
	tool := NewPostMessageTool(fp, fakeResolver{members: []string{"Tickets", "Bootcamp"}}, nil)
	_, err := tool.Run(context.Background(), json.RawMessage(`{"text":"x"}`))
	if err == nil {
		t.Fatal("missing channel should error")
	}
	if !strings.Contains(err.Error(), "Tickets") || !strings.Contains(err.Error(), "Bootcamp") {
		t.Fatalf("error should list member channels, got %v", err)
	}
	if fp.calls != 0 {
		t.Fatalf("no post should happen, got %d", fp.calls)
	}
}

func TestPostMessageErrors(t *testing.T) {
	fp := &fakePoster{}
	tool := NewPostMessageTool(fp, fakeResolver{ids: map[string]string{"a": "id-a"}}, nil)
	// Unknown channel.
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"channel":"nope","text":"x"}`)); err == nil {
		t.Error("unknown channel should error")
	}
	// Missing text.
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"channel":"a"}`)); err == nil {
		t.Error("missing text should error")
	}
	if fp.calls != 0 {
		t.Fatalf("no post should have happened on error paths, got %d", fp.calls)
	}
}
