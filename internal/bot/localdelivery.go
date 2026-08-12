package bot

import (
	"context"
	"strings"
)

// LocalDelivery hands a message to a teammate running in this process, in
// addition to writing it to the store.
//
// The store is still where the message is recorded - the operator reads the
// thread, not the process - but it is no longer what wakes the teammate. That
// used to matter because the websocket could be down; it matters less now, and
// what it still buys is latency: the teammate starts before the write's own
// fan-out has reached them.
//
// Both copies carry the same message sequence and the receiving runtime acts on
// whichever arrives first, so a teammate acts once either way.
//
// A nil *LocalDelivery disables all of it, which is what a lone bot wants.
type LocalDelivery struct {
	// Self is this bot's name, so it never delivers to itself.
	Self string
	// SelfActor is this bot's actor id. It becomes the delivered message's
	// author, so the receiving bot resolves the sender exactly as it would for
	// the same message arriving from the store.
	SelfActor string
	Fleet     *Fleet
}

// target returns the teammate name to deliver to, or "" when there is none.
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

// busyNote describes a teammate that is mid-turn, for the tool result the model
// reads. Knowing the teammate is already working is what stops a second ping.
func (d *LocalDelivery) busyNote(to string) string {
	if d == nil || !d.Fleet.Busy(to) {
		return ""
	}
	return " (they are mid-turn; they will pick this up when it ends - do not ping again)"
}

// sayAndDeliver writes text into a thread and, when the recipient runs in this
// process, hands them the same message directly. It reports whether the local
// delivery happened.
//
// The write comes first and its failure is the caller's failure: a message
// delivered in-process but missing from the thread would be invisible to the
// operator reading along, which is worse than not sending it.
func sayAndDeliver(ctx context.Context, w ThreadWriter, d *LocalDelivery, to string,
	thread ThreadID, text string, o SayOpts) (bool, error) {
	seq, err := w.Say(ctx, thread, text, o)
	if err != nil {
		return false, err
	}
	local := d.target(to)
	if local == "" {
		return false, nil
	}
	// The sequence is what makes the two copies one message. Without it the
	// recipient acts on both: the store's own fan-out delivers the written
	// message, and this hands them a second copy with no identity to compare.
	return d.deliver(ctx, local, Inbound{
		Kind: "mention", Thread: thread, Author: d.SelfActor, Text: text, MessageSeq: seq,
	}), nil
}

// deliver hands the message to the named teammate and reports whether it
// landed.
func (d *LocalDelivery) deliver(ctx context.Context, to string, in Inbound) bool {
	if d == nil || d.Fleet == nil {
		return false
	}
	return d.Fleet.Deliver(ctx, to, in)
}
