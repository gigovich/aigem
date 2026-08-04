package mattermost

import (
	"encoding/json"
	"regexp"

	"github.com/gigovich/aigem/internal/bot"
)

// mentionEdge is the character class a real "@mention" may follow: start of string or any
// non username/email character. It rejects "name@here.com" (the "@" follows a username char)
// while still matching mentions wrapped in punctuation like "(@here)" or a ">" quote.
const mentionEdge = `(^|[^a-zA-Z0-9._@-])`

// broadcastRe matches the group-mention tokens (@here, @channel, @all) as standalone words.
var broadcastRe = regexp.MustCompile(`(?i)` + mentionEdge + `@(here|channel|all)($|\W)`)

// wsEvent is one frame from the Mattermost event WebSocket.
type wsEvent struct {
	Event string                     `json:"event"`
	Data  map[string]json.RawMessage `json:"data"`
	Seq   int64                      `json:"seq"`
}

// wsPost is the subset of a Mattermost post the bot needs.
type wsPost struct {
	ID        string   `json:"id"`
	ChannelID string   `json:"channel_id"`
	RootID    string   `json:"root_id"`
	UserID    string   `json:"user_id"`
	Message   string   `json:"message"`
	FileIDs   []string `json:"file_ids"`
	Props     struct {
		// Mattermost stamps every post made through a bot account with from_bot="true".
		FromBot string `json:"from_bot"`
	} `json:"props"`
}

// parsePost extracts the post and channel type from a posted event. ok is false for
// non-posted events and malformed payloads.
func parsePost(e wsEvent) (wsPost, string, bool) {
	if e.Event != "posted" {
		return wsPost{}, "", false
	}
	var postStr string
	if err := json.Unmarshal(e.Data["post"], &postStr); err != nil {
		return wsPost{}, "", false
	}
	var p wsPost
	if err := json.Unmarshal([]byte(postStr), &p); err != nil {
		return wsPost{}, "", false
	}
	var channelType string
	if raw, ok := e.Data["channel_type"]; ok {
		_ = json.Unmarshal(raw, &channelType)
	}
	return p, channelType, true
}

// classifyPost turns a parsed post into an Inbound the bot should act on IMMEDIATELY: a direct
// message, an explicit mention, or a broadcast. Returns ok=false for the bot's own posts and
// for any other post - including a plain reply in a thread the bot is in, which the transport
// instead routes through the debouncer so the owner reacts to the whole burst at once rather
// than per message.
func classifyPost(p wsPost, channelType string, mentionsRaw json.RawMessage, botUserID string) (bot.Inbound, bool) {
	if p.UserID == botUserID {
		return bot.Inbound{}, false
	}
	in := bot.Inbound{
		Channel: p.ChannelID,
		Thread:  bot.ThreadRef{ChannelID: p.ChannelID, RootID: p.RootID},
		Author:  p.UserID,
		Text:    p.Message,
		FileIDs: p.FileIDs,
		PostID:  p.ID,
	}
	if channelType == "D" {
		in.Kind = "dm"
		return in, true
	}
	// A mention of (or broadcast to) a root-level post must reply with root_id = that post's
	// id so the reply opens a thread (and future replies are tracked as the bot's thread).
	if mentioned(mentionsRaw, botUserID) {
		if in.Thread.RootID == "" {
			in.Thread.RootID = p.ID
		}
		in.Kind = "mention"
		return in, true
	}
	if broadcastRe.MatchString(p.Message) {
		if in.Thread.RootID == "" {
			in.Thread.RootID = p.ID
		}
		in.Kind = "broadcast"
		return in, true
	}
	return bot.Inbound{}, false
}

// mentionIDs decodes the mentions field, which arrives as a JSON-encoded string holding a
// JSON array of user ids.
func mentionIDs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(s), &ids); err != nil {
		return nil
	}
	return ids
}

func mentioned(raw json.RawMessage, botUserID string) bool {
	for _, id := range mentionIDs(raw) {
		if id == botUserID {
			return true
		}
	}
	return false
}
