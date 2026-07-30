package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gigovich/aigem/internal/tools"
)

// ChatReader reads chat the bot was not woken in: a channel's recent history, or one whole
// thread. *mattermost.Transport satisfies it.
type ChatReader interface {
	ChannelDigest(ctx context.Context, channelID string, limit int) (string, error)
	ThreadText(ctx context.Context, channelID, rootID string) (string, error)
}

type readChatTool struct {
	reader   ChatReader
	resolver ChannelResolver
}

// NewReadChatTool lets the bot read a channel or thread on demand. Without it a bot woken in one
// place cannot look at another - so "re-read that thread" was unexecutable and the only way out
// was asking a human to quote the messages back.
func NewReadChatTool(reader ChatReader, resolver ChannelResolver) tools.Tool {
	return &readChatTool{reader: reader, resolver: resolver}
}

func (t *readChatTool) Name() string       { return "read_chat" }
func (t *readChatTool) NeedsConfirm() bool { return false }

func (t *readChatTool) Description() string {
	return "Read chat you were not woken in. Args: channel (required - a channel name you are a " +
		"member of, or \"@username\" for a direct conversation), thread (optional - a thread root " +
		"post id), limit (optional - how many recent posts, default 60, and it applies to channel reads " +
		"only: a thread always comes back whole). Without thread you get the " +
		"channel's recent posts, tagged with the id of the thread they belong to wherever it changes; " +
		"pass one of those ids as thread to read that whole conversation. Use it before you answer a question " +
		"about another conversation, and before you repeat a handoff or a question - the answer you " +
		"are waiting for may already be there. Never ask a person to quote a message you can read."
}

func (t *readChatTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"channel":{"type":"string","description":"name of a channel you are a member of, or @username for a direct conversation"},
			"thread":{"type":"string","description":"optional thread root post id to read that whole thread"},
			"limit":{"type":"integer","description":"optional number of recent posts to read (default 60, max 200)"}
		},
		"required":["channel"]
	}`)
}

// maxReadChatLimit caps how many posts one read may pull, so a single call cannot flood the turn.
const maxReadChatLimit = 200

// postIDLen is the length of a Mattermost id.
const postIDLen = 26

// isPostID reports whether s has the shape of a Mattermost post id: 26 lowercase alphanumerics.
func isPostID(s string) bool {
	if len(s) != postIDLen {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func (t *readChatTool) Run(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var a struct {
		Channel string `json:"channel"`
		Thread  string `json:"thread"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Channel) == "" {
		names, err := t.resolver.MemberChannels(ctx)
		if err != nil {
			return "", fmt.Errorf("specify a channel (could not list your channels: %w)", err)
		}
		return "", fmt.Errorf("specify a channel (or @username); you are a member of: %s",
			strings.Join(names, ", "))
	}
	id, err := t.resolver.ResolveChannel(ctx, a.Channel)
	if err != nil {
		return "", err
	}
	if root := strings.TrimSpace(a.Thread); root != "" {
		// Check the shape here rather than paying a round trip to learn the id was invented: a
		// model with no id at hand tends to supply a plausible-looking number.
		if !isPostID(root) {
			return "", fmt.Errorf("%q is not a thread id; ids are 26 letters and digits and come "+
				"from reading the channel first, so read %s without a thread and take one from there",
				root, a.Channel)
		}
		text, err := t.reader.ThreadText(ctx, id, root)
		if err != nil {
			return "", fmt.Errorf("read thread %s: %w", root, err)
		}
		if text == "" {
			return fmt.Sprintf("thread %s in %q is empty", root, a.Channel), nil
		}
		return text, nil
	}
	if a.Limit <= 0 {
		a.Limit = 60
	}
	if a.Limit > maxReadChatLimit {
		a.Limit = maxReadChatLimit
	}
	text, err := t.reader.ChannelDigest(ctx, id, a.Limit)
	if err != nil {
		return "", fmt.Errorf("read channel %q: %w", a.Channel, err)
	}
	if text == "" {
		return fmt.Sprintf("no recent posts in %q", a.Channel), nil
	}
	return text, nil
}
