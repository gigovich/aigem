package bot

import (
	"context"

	"github.com/gigovich/aigem/internal/llm"
)

// ThreadRef identifies the chat thread a message belongs to.
type ThreadRef struct {
	ChannelID string
	RootID    string // empty for a root-level post
}

// Inbound is a message routed to the bot by its transport. "resume" is a
// runtime-internal kind: an automatic continuation of a turn that was cut off
// by a budget or a transient provider failure.
type Inbound struct {
	Kind    string // "mention" | "dm" | "broadcast" | "thread_update" | "resume"
	Channel string
	Thread  ThreadRef
	Author  string
	Text    string
	FileIDs []string // transport ids of the message's file attachments
}

// Transport is a chat backend the bot communicates through.
type Transport interface {
	Events() <-chan Inbound
	Reply(thread ThreadRef, text string) error
	Post(channel, text string) error
	Close() error
}

// HistoryReader, when implemented by a Transport, returns a formatted block of recent
// channel messages the bot was not addressed in, or "" when there are none.
type HistoryReader interface {
	History(ctx context.Context, channelID string) string
}

// Typist, when implemented by a Transport, signals that the bot is composing a reply in the
// given thread. The signal is transient and must be re-sent periodically by the caller.
type Typist interface {
	Typing(thread ThreadRef) error
}

// ThreadReader, when implemented by a Transport, returns the full thread (root plus all
// replies) as a formatted block, for a "thread_update" run where the bot owns or is in the
// thread and must read everything that was said to decide whether to respond.
type ThreadReader interface {
	ThreadHistory(ctx context.Context, channelID, rootID string) string
}

// AuthorNamer, when implemented by a Transport, resolves a message author's id
// to a display name. Who is speaking is part of what a message means - a hold
// from a human is not a suggestion from a peer bot - so it is worth resolving
// wherever the author reaches the model as a bare id.
type AuthorNamer interface {
	AuthorName(ctx context.Context, userID string) string
}

// AttachmentFetcher, when implemented by a Transport, resolves a message's file
// attachments: viewable images are downloaded and returned for a multimodal
// turn, and note describes every attachment (including ones that could not be
// fetched or viewed) so the model knows what arrived. Both may be empty.
type AttachmentFetcher interface {
	Attachments(ctx context.Context, fileIDs []string) (images []llm.Image, note string)
}
