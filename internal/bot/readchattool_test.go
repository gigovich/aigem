package bot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeChatReader struct {
	digest      string
	digestErr   error
	thread      string
	threadErr   error
	gotChannel  string
	gotRoot     string
	gotLimit    int
	digestCalls int
	threadCalls int
}

func (f *fakeChatReader) ChannelDigest(_ context.Context, channelID string, limit int) (string, error) {
	f.digestCalls++
	f.gotChannel, f.gotLimit = channelID, limit
	return f.digest, f.digestErr
}

func (f *fakeChatReader) ThreadText(_ context.Context, channelID, rootID string) (string, error) {
	f.threadCalls++
	f.gotChannel, f.gotRoot = channelID, rootID
	return f.thread, f.threadErr
}

func runReadChat(t *testing.T, r *fakeChatReader, res ChannelResolver, args string) (string, error) {
	t.Helper()
	return NewReadChatTool(r, res).Run(context.Background(), json.RawMessage(args))
}

// tasksResolver resolves the one channel these tests read from.
func tasksResolver() fakeResolver {
	return fakeResolver{ids: map[string]string{"Tasks": "chan1"}, members: []string{"Tasks", "Log"}}
}

func TestReadChatReadsChannelDigest(t *testing.T) {
	r := &fakeChatReader{digest: "amiran: [thread abc] статус"}
	out, err := runReadChat(t, r, tasksResolver(), `{"channel":"Tasks"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != r.digest {
		t.Fatalf("out = %q", out)
	}
	if r.gotChannel != "chan1" {
		t.Fatalf("channel resolved to %q, want chan1", r.gotChannel)
	}
	if r.gotLimit != 60 {
		t.Fatalf("default limit = %d, want 60", r.gotLimit)
	}
	if r.threadCalls != 0 {
		t.Fatal("no thread was requested, so ThreadText must not be called")
	}
}

func TestReadChatReadsOneThread(t *testing.T) {
	r := &fakeChatReader{thread: "kate: готова взять DOAML-7"}
	out, err := runReadChat(t, r, tasksResolver(), `{"channel":"Tasks","thread":"yxy9zt7jpjrq5cgoydybc5q14r"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != r.thread {
		t.Fatalf("out = %q", out)
	}
	if r.gotRoot != "yxy9zt7jpjrq5cgoydybc5q14r" {
		t.Fatalf("root = %q", r.gotRoot)
	}
	if r.digestCalls != 0 {
		t.Fatal("an explicit thread must not also pull the channel digest")
	}
}

func TestReadChatClampsLimit(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, 60}, {-5, 60}, {10, 10}, {5000, maxReadChatLimit}} {
		r := &fakeChatReader{digest: "x"}
		args, _ := json.Marshal(map[string]any{"channel": "Tasks", "limit": tc.in})
		if _, err := runReadChat(t, r, tasksResolver(), string(args)); err != nil {
			t.Fatal(err)
		}
		if r.gotLimit != tc.want {
			t.Errorf("limit %d -> %d, want %d", tc.in, r.gotLimit, tc.want)
		}
	}
}

func TestReadChatEmptyResultsAreExplicit(t *testing.T) {
	// Silence would read as "there is nothing there"; the agent must be told which case it hit.
	out, err := runReadChat(t, &fakeChatReader{}, tasksResolver(), `{"channel":"Tasks"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no recent posts") {
		t.Fatalf("out = %q", out)
	}
	out, err = runReadChat(t, &fakeChatReader{}, tasksResolver(), `{"channel":"Tasks","thread":"yxy9zt7jpjrq5cgoydybc5q14r"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "is empty") {
		t.Fatalf("out = %q", out)
	}
}

func TestReadChatMissingChannelListsMemberships(t *testing.T) {
	_, err := runReadChat(t, &fakeChatReader{}, tasksResolver(), `{"channel":"  "}`)
	if err == nil {
		t.Fatal("expected an error naming the channels the bot belongs to")
	}
	if !strings.Contains(err.Error(), "Tasks") || !strings.Contains(err.Error(), "Log") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadChatPropagatesErrors(t *testing.T) {
	if _, err := runReadChat(t, &fakeChatReader{}, tasksResolver(), `{"channel":"Secret"}`); err == nil {
		t.Fatal("an unresolvable channel must error")
	}
	r := &fakeChatReader{digestErr: errors.New("boom")}
	if _, err := runReadChat(t, r, tasksResolver(), `{"channel":"Tasks"}`); err == nil {
		t.Fatal("a failed digest must error, not return empty")
	}
	r = &fakeChatReader{threadErr: errors.New("boom")}
	if _, err := runReadChat(t, r, tasksResolver(), `{"channel":"Tasks","thread":"yxy9zt7jpjrq5cgoydybc5q14r"}`); err == nil {
		t.Fatal("a failed thread read must error, not return empty")
	}
}

func TestReadChatIsAvailableToEveryRole(t *testing.T) {
	// A bot that cannot read chat has to ask a human to quote messages back to it.
	for _, r := range Roles() {
		found := false
		for _, name := range r.Allow {
			if name == "read_chat" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("role %q cannot use read_chat", r.Name)
		}
	}
}

// A model with no id at hand tends to invent a plausible-looking number; refuse it locally rather
// than paying a round trip to the server to learn the same thing.
func TestReadChatRejectsAnInventedThreadID(t *testing.T) {
	r := &fakeChatReader{thread: "should not be reached"}
	for _, bad := range []string{"1784751462651167393", "abc", "ROOT9", "root9!", ""} {
		args, _ := json.Marshal(map[string]any{"channel": "Tasks", "thread": bad})
		out, err := runReadChat(t, r, tasksResolver(), string(args))
		if bad == "" {
			// An empty thread means "read the channel", not an invalid id.
			if err != nil {
				t.Fatalf("empty thread should fall back to a channel read: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("thread %q was accepted, got %q", bad, out)
			continue
		}
		if !strings.Contains(err.Error(), "26 letters and digits") {
			t.Errorf("thread %q: error should explain the id shape, got %v", bad, err)
		}
	}
	if r.threadCalls != 0 {
		t.Fatalf("an invalid id must not reach the transport, got %d calls", r.threadCalls)
	}
}

func TestReadChatAcceptsARealThreadID(t *testing.T) {
	r := &fakeChatReader{thread: "kate: готова взять DOAML-7"}
	args, _ := json.Marshal(map[string]any{"channel": "Tasks", "thread": "yxy9zt7jpjrq5cgoydybc5q14r"})
	out, err := runReadChat(t, r, tasksResolver(), string(args))
	if err != nil {
		t.Fatal(err)
	}
	if out != r.thread {
		t.Fatalf("out = %q", out)
	}
}
