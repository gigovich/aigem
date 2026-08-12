package bot

import (
	"context"

	"github.com/gigovich/aigem/internal/agent"
	"github.com/gigovich/aigem/internal/llm"
)

// ThreadID identifies a conversation. It replaces the channel-and-root pair the
// Mattermost transport needed: with no channels, there is nothing above a
// thread left to name.
type ThreadID string

// Inbound is a message routed to the bot by its transport.
type Inbound struct {
	// Kind is one of:
	//
	//   "mention"       - a participant addressed this bot; answer.
	//   "thread_update" - a thread it is in moved without addressing it; decide
	//                     whether it has anything to add.
	//   "resume"        - a runtime-internal continuation of a turn cut short by
	//                     a budget or a transient provider failure.
	Kind   string
	Thread ThreadID
	// Author is an actor id: "human:operator" or "bot:jane".
	Author string
	Text   string
	// AttachmentIDs are the files on the message, resolved by AttachmentFetcher.
	AttachmentIDs []string
	// MessageSeq is the store sequence of the message this came from, so a copy
	// delivered in-process and the same message arriving from the store's
	// publisher are acted on once. Zero for anything with no message behind it.
	MessageSeq uint64
}

// Transport is the conversation backend the bot talks through.
type Transport interface {
	Events() <-chan Inbound
	Reply(thread ThreadID, text string) error
	Close() error
}

// SayOpts carries what a message means beyond its text.
//
// AwaitReply marks the thread as needing the operator, which is what puts it at
// the top of their inbox with the accent marker. It is explicit rather than
// inferred: guessing from a trailing question mark is how a bot ends up
// shouting for attention it does not need.
type SayOpts struct {
	AwaitReply bool
	Mentions   []ThreadActor
}

// ThreadActor is a participant id as the transport speaks it.
type ThreadActor = string

// ThreadWriter writes to threads other than the one a turn was woken in, and
// opens new ones. It replaces the channel-shaped Poster and ChannelResolver:
// creating a conversation now means naming its participants, which a channel
// never required.
type ThreadWriter interface {
	Say(ctx context.Context, thread ThreadID, text string, o SayOpts) error
	Open(ctx context.Context, title string, participants []ThreadActor, text string) (ThreadID, error)
	Join(ctx context.Context, thread ThreadID, actor ThreadActor) error
	// ActorFor maps a teammate's name to the id the store knows them by, or ""
	// when no such actor exists. A model naming someone who is not in the fleet
	// must be told so, not have the message land nowhere.
	ActorFor(name string) ThreadActor
}

// ThreadHistoryReader renders one thread for the model, for a turn that has to
// read everything that was said to decide whether to answer.
//
// It is separate from ThreadReader because the runtime needs only this, and an
// interface it does not use is one a transport has to implement to be usable at
// all - which is how a capability check quietly becomes a build requirement.
type ThreadHistoryReader interface {
	ThreadHistory(ctx context.Context, thread ThreadID) string
}

// ThreadReader is what the tools read through: everything a bot can look up
// about the conversations it is in.
type ThreadReader interface {
	ThreadHistoryReader
	// ThreadText is a thread as a tool result, with an error the model can act
	// on when it is not one of the bot's own.
	ThreadText(ctx context.Context, thread ThreadID) (string, error)
	// Threads lists the bot's own conversations, optionally filtered by state.
	Threads(ctx context.Context, state string, limit int) (string, error)
	// Search runs a full-text query over the messages in those threads. It is
	// the replacement for the ambient channel history a bot used to be handed
	// unasked: on demand, and scoped to what it is in.
	Search(ctx context.Context, query string, limit int) (string, error)
}

// AuthorNamer resolves a message author's id to a display name. Who is speaking
// is part of what a message means - a hold from a person is not a suggestion
// from a peer bot - so it is worth resolving wherever the author reaches the
// model as a bare id.
type AuthorNamer interface {
	AuthorName(ctx context.Context, actorID string) string
}

// AttachmentFetcher resolves a message's files: viewable images are returned
// for a multimodal turn, and note describes every attachment - including the
// ones that could not be viewed - so the model knows what arrived rather than
// guessing whether it can see something. Both may be empty.
type AttachmentFetcher interface {
	Attachments(ctx context.Context, ids []string) (images []llm.Image, note string)
}

// Journaller returns the sink that records a turn's steps into the thread's
// timeline, and the function that closes it. This is what puts the tool calls,
// the diffs and the plan in front of the operator instead of only in the log.
//
// A transport that does not implement it still works; the turn is then visible
// only as its answer, which is all Mattermost ever showed.
type Journaller interface {
	TurnEvents(thread ThreadID, actor string) (ev agent.Events, done func(answer string, err error))
}
