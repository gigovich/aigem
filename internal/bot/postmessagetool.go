package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// Poster sends a message to a channel id, or into a specific thread of it. *mattermost.Transport
// satisfies it.
type Poster interface {
	Post(channel, text string) error
	PostToThread(channelID, rootID, text string) error
}

// ChannelResolver maps a channel name to its id among the channels the bot is a member of,
// and lists those memberships. Membership in Mattermost is the single source of truth for
// where a bot may post, so there is no separate configured channel list.
type ChannelResolver interface {
	ResolveChannel(ctx context.Context, name string) (channelID string, err error)
	MemberChannels(ctx context.Context) (names []string, err error)
}

type postMessageTool struct {
	poster   Poster
	resolver ChannelResolver
}

// NewPostMessageTool lets the bot post to one of the channels it belongs to. Not confirm-gated;
// Mattermost membership is the safety boundary - the bot can only post where it was invited.
func NewPostMessageTool(poster Poster, resolver ChannelResolver) tools.Tool {
	return &postMessageTool{poster: poster, resolver: resolver}
}

func (t *postMessageTool) Name() string       { return "post_message" }
func (t *postMessageTool) NeedsConfirm() bool { return false }

func (t *postMessageTool) Description() string {
	return "Post a message to one of the channels you belong to, or a direct message. Args: " +
		"channel (required - a channel name, or \"@username\" to send a direct message to that " +
		"user), text (required), and thread (optional - a thread root post id to reply " +
		"into that existing thread instead of starting a new one). A named channel must be one you " +
		"are a member of. Use this to deliver a scheduled run's result: pass thread to land it back " +
		"in the thread the work was requested in, or \"@username\" to deliver it to the DM " +
		"conversation it was requested in. When replying to someone live, answer in their " +
		"thread instead."
}

func (t *postMessageTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"channel":{"type":"string","description":"name of a channel you are a member of, or @username for a direct message"},
			"text":{"type":"string","description":"the message to post"},
			"thread":{"type":"string","description":"optional thread root post id to reply into an existing thread"}
		},
		"required":["channel","text"]
	}`)
}

func (t *postMessageTool) Run(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
		Thread  string `json:"thread"`
	}
	if err := json.Unmarshal(rawArgs, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Text) == "" {
		return "", fmt.Errorf("text is required")
	}
	if strings.TrimSpace(a.Channel) == "" {
		names, err := t.resolver.MemberChannels(ctx)
		if err != nil {
			return "", fmt.Errorf("specify a channel (could not list your channels: %w)", err)
		}
		return "", fmt.Errorf("specify a channel (or @username for a DM); you are a member of: %s",
			strings.Join(names, ", "))
	}
	id, err := t.resolver.ResolveChannel(ctx, a.Channel)
	if err != nil {
		return "", err
	}
	if root := strings.TrimSpace(a.Thread); root != "" {
		if err := t.poster.PostToThread(id, root, a.Text); err != nil {
			return "", err
		}
		return fmt.Sprintf("posted to %q thread %s", a.Channel, root), nil
	}
	if err := t.poster.Post(id, a.Text); err != nil {
		return "", err
	}
	return fmt.Sprintf("posted to %q", a.Channel), nil
}
