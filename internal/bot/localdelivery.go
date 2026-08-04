package bot

import (
	"context"
	"strings"
)

// IDPoster is a Poster that can report the ids of the posts it wrote - one per chunk, since a
// long message is split. *mattermost.Transport satisfies it. A Poster that does not is still
// usable: local delivery is then skipped, and the chat post remains the only path.
type IDPoster interface {
	PostWithIDs(channelID, rootID, text string) (postIDs []string, err error)
}

// LocalDelivery hands a message to a teammate running in this process, in
// addition to writing it to chat.
//
// Chat is still where the message is recorded - a human reads the channel, not
// the process - but it is no longer what wakes the teammate. That matters when
// the websocket is down or reconnecting, which is exactly when a handoff must
// not be lost. Both copies carry the same post id and the receiving runtime acts
// on whichever arrives first.
//
// A nil *LocalDelivery disables all of it, which is what a lone bot wants.
type LocalDelivery struct {
	// Self is this bot's name, so it never delivers to itself.
	Self string
	// SelfUserID is this bot's chat user id. It becomes the delivered message's
	// author, so the receiving bot resolves the sender's name exactly as it would
	// for the same message arriving over the websocket.
	SelfUserID string
	Fleet      *Fleet
}

// target returns the teammate name to deliver to, or "" when there is none.
//
// Matching goes through Fleet.Resolve, which compares the chat username the caller addressed
// against each bot's chat account rather than its aigem name: a bot whose aigem name happens to
// equal some person's username must not receive that person's messages. A name no bot in this
// process answers to finds nothing, and the chat post remains the only path - the pre-fleet
// behaviour, which still works.
func (d *LocalDelivery) target(to string) string {
	if d == nil || d.Fleet == nil {
		return ""
	}
	name := strings.TrimPrefix(strings.TrimSpace(to), "@")
	if name == "" || strings.EqualFold(name, d.Self) {
		return ""
	}
	return d.Fleet.Resolve(name)
}

// deliver hands the message to the named teammate and reports whether it landed. postIDs are the
// chat posts carrying the same text; the first identifies this delivery, and the rest are marked
// as already handled so a chunked message does not wake the teammate again on a partial copy.
func (d *LocalDelivery) deliver(ctx context.Context, to, channel string, in Inbound, postIDs []string) bool {
	if d == nil || d.Fleet == nil {
		return false
	}
	if len(postIDs) > 0 {
		in.PostID = postIDs[0]
	}
	in.Author = d.SelfUserID
	if !d.Fleet.Deliver(ctx, to, channel, in) {
		return false
	}
	// Only once the teammate has the message do the remaining chunks count as handled; marking
	// them after a refused delivery would suppress the chat copies that are then the only path.
	if len(postIDs) > 1 {
		d.Fleet.MarkRouted(to, postIDs[1:])
	}
	return true
}

// busyNote describes a teammate that is mid-turn, for the tool result the model
// reads. Knowing the teammate is already working is what stops a second ping.
func (d *LocalDelivery) busyNote(to string) string {
	if d == nil || !d.Fleet.Busy(to) {
		return ""
	}
	return " (they are mid-turn; they will pick this up when it ends - do not ping again)"
}

// postAndDeliver writes text to chat and, when the recipient runs in this process, hands them the
// same message directly. It reports whether local delivery happened.
//
// The chat write comes first and its failure is the tool's failure: a message delivered
// in-process but missing from the channel would be invisible to the human reading along, which is
// worse than not sending it.
//
// dm says the target channel is a direct conversation. It matters because the in-process copy
// must be the message the teammate would have received over the websocket, down to its thread:
// the transport classifies a direct message as "dm" and leaves it at conversation root, while a
// channel post opens a thread at its own id. Building it differently here would split one
// conversation across two per-thread agents, and which one ran would depend on which copy won
// the race.
func postAndDeliver(ctx context.Context, poster Poster, d *LocalDelivery, to, channel, channelID,
	rootID string, dm bool, text string) (bool, error) {
	local := d.target(to)
	idp, canID := poster.(IDPoster)
	if local == "" || !canID {
		if rootID != "" {
			return false, poster.PostToThread(channelID, rootID, text)
		}
		return false, poster.Post(channelID, text)
	}
	ids, err := idp.PostWithIDs(channelID, rootID, text)
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, nil // nothing was posted, so there is nothing to deliver
	}
	in := Inbound{Kind: "mention", Channel: channelID, Text: text,
		Thread: ThreadRef{ChannelID: channelID, RootID: rootID}}
	if dm {
		in.Kind = "dm"
	} else if in.Thread.RootID == "" {
		// A new root post is its own thread, so the teammate answers in it rather than starting
		// another one - which is exactly what the transport does with a mention at root level.
		in.Thread.RootID = ids[0]
	}
	return d.deliver(ctx, local, channel, in, ids), nil
}
