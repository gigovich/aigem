package mattermost

import (
	"encoding/json"
	"testing"

	"github.com/gigovich/aigem/internal/bot"
)

func ev(t *testing.T, data map[string]any) wsEvent {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for k, v := range data {
		b, _ := json.Marshal(v)
		raw[k] = b
	}
	return wsEvent{Event: "posted", Data: raw}
}

func postJSON(channelID, rootID, userID, msg string) string {
	b, _ := json.Marshal(map[string]string{
		"id": "p1", "channel_id": channelID, "root_id": rootID, "user_id": userID, "message": msg,
	})
	return string(b)
}

func classifyEvent(e wsEvent, botUserID string) (bot.Inbound, bool) {
	p, ct, ok := parsePost(e)
	if !ok {
		return bot.Inbound{}, false
	}
	return classifyPost(p, ct, e.Data["mentions"], botUserID)
}

func TestClassifyCarriesFileIDs(t *testing.T) {
	b, _ := json.Marshal(map[string]any{
		"id": "p1", "channel_id": "c1", "user_id": "u-x", "message": "смотри скриншот",
		"file_ids": []string{"f1", "f2"},
	})
	e := ev(t, map[string]any{"channel_type": "O", "mentions": `["bot123"]`, "post": string(b)})
	in, ok := classifyEvent(e, "bot123")
	if !ok || len(in.FileIDs) != 2 || in.FileIDs[0] != "f1" {
		t.Fatalf("classify with attachments = %+v, ok=%v", in, ok)
	}
}

func TestParsePostIgnoresNonPosted(t *testing.T) {
	e := ev(t, map[string]any{"channel_type": "O", "post": postJSON("c1", "", "u-x", "hi")})
	e.Event = "typing"
	if _, _, ok := parsePost(e); ok {
		t.Error("non-posted event should not parse")
	}
}

func TestClassifyDM(t *testing.T) {
	e := ev(t, map[string]any{"channel_type": "D", "post": postJSON("c1", "", "u-someone", "hi")})
	in, ok := classifyEvent(e, "bot123")
	if !ok || in.Kind != "dm" || in.Text != "hi" || in.Thread.ChannelID != "c1" {
		t.Fatalf("classify DM = %+v, ok=%v", in, ok)
	}
}

func TestClassifyMention(t *testing.T) {
	e := ev(t, map[string]any{
		"channel_type": "O", "mentions": `["bot123"]`,
		"post": postJSON("c1", "", "u-someone", "@amiran help"),
	})
	in, ok := classifyEvent(e, "bot123")
	if !ok || in.Kind != "mention" || in.Thread.RootID != "p1" {
		t.Fatalf("classify mention = %+v, ok=%v", in, ok)
	}
}

func TestClassifyBroadcastAddressesEveryone(t *testing.T) {
	for _, token := range []string{"@here", "@channel", "@all"} {
		e := ev(t, map[string]any{
			"channel_type": "O",
			"post":         postJSON("c1", "", "u-someone", token+" standup time"),
		})
		in, ok := classifyEvent(e, "bot123")
		if !ok || in.Kind != "broadcast" || in.Thread.RootID != "p1" {
			t.Fatalf("classify %s = %+v, ok=%v", token, in, ok)
		}
	}
}

// A plain reply in a thread is not classified as an immediate action: the transport routes it
// through the debouncer instead (see TestDispatchObservesThreadReply). classifyPost only emits
// the directly addressed forms.
func TestClassifyPlainThreadReplyIsNotImmediate(t *testing.T) {
	e := ev(t, map[string]any{"channel_type": "O", "post": postJSON("c1", "root1", "u-someone", "more")})
	if in, ok := classifyEvent(e, "bot123"); ok {
		t.Fatalf("plain thread reply should not be immediate, got %+v", in)
	}
}

func TestClassifyBroadcastInThreadStillReplies(t *testing.T) {
	e := ev(t, map[string]any{
		"channel_type": "O",
		"post":         postJSON("c1", "root1", "u-someone", "@amiran and @here too"),
	})
	in, ok := classifyEvent(e, "bot123")
	if !ok || in.Kind != "broadcast" {
		t.Fatalf("broadcast in thread = %+v, ok=%v", in, ok)
	}
}

func TestClassifyBroadcastInParens(t *testing.T) {
	e := ev(t, map[string]any{
		"channel_type": "O",
		"post":         postJSON("c1", "", "u-someone", "(@here) пинг"),
	})
	in, ok := classifyEvent(e, "bot123")
	if !ok || in.Kind != "broadcast" {
		t.Fatalf("parenthesized broadcast = %+v, ok=%v", in, ok)
	}
}

func postJSONFromBot(channelID, rootID, userID, msg string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "p1", "channel_id": channelID, "root_id": rootID, "user_id": userID,
		"message": msg, "props": map[string]string{"from_bot": "true"},
	})
	return string(b)
}

func TestClassifyBotMentionStillReplies(t *testing.T) {
	e := ev(t, map[string]any{
		"channel_type": "O", "mentions": `["bot123"]`,
		"post": postJSONFromBot("c1", "root1", "other-bot", "@me глянь"),
	})
	in, ok := classifyEvent(e, "bot123")
	if !ok || in.Kind != "mention" {
		t.Fatalf("explicit mention from a bot should still reply = %+v, ok=%v", in, ok)
	}
}

func TestClassifyDoesNotMatchEmailLikeHere(t *testing.T) {
	e := ev(t, map[string]any{
		"channel_type": "O",
		"post":         postJSON("c1", "", "u-someone", "ping me at bob@here.com"),
	})
	if _, ok := classifyEvent(e, "bot123"); ok {
		t.Error("email-like @here.com should not be treated as a broadcast")
	}
}

func TestClassifyIgnoresOwnAndUnaddressed(t *testing.T) {
	own := ev(t, map[string]any{"channel_type": "O", "post": postJSON("c1", "", "bot123", "mine")})
	if _, ok := classifyEvent(own, "bot123"); ok {
		t.Error("own message should be ignored")
	}
	chatter := ev(t, map[string]any{"channel_type": "O", "post": postJSON("c1", "", "u-x", "noise")})
	if _, ok := classifyEvent(chatter, "bot123"); ok {
		t.Error("unaddressed channel chatter should be ignored")
	}
}
