package chatlink

import (
	"github.com/gigovich/aigem/internal/chat"
)

// What a committed message means to one bot.
const (
	kindIgnore = iota
	kindMention
	kindUpdate
)

// classify decides whether a message wakes this bot now, later, or not at all.
//
// It needs no query: the frame carries the message and the thread's
// participants as of the write, which is exactly the two facts the decision
// rests on. The rules, and why each is here:
//
//   - A message this bot wrote is never work for it. Without this rule two bots
//     in one thread answer each other forever.
//   - A message that names it is work now. This is the only deliberate way to
//     address a bot, and it replaces the chat mention that used to be a
//     substring match against a display name.
//   - A message from a person in a thread where this bot is the only one is
//     work now too. That is a direct conversation by any other name, and making
//     it wait out a quiet period would put a 45-second pause into every
//     exchange with the operator.
//   - Anything else is a thread update: the conversation moved without
//     addressing this bot, and it decides for itself whether it has anything to
//     add - after the thread goes quiet, so one burst is one decision.
func classify(m chat.Message, audience []string, self string) int {
	if m.Author == self {
		return kindIgnore
	}
	// A system note is the store describing itself - someone joined, someone
	// left. It is in the transcript for the record, not to be answered.
	if m.Kind == chat.MsgSystem {
		return kindIgnore
	}
	for _, id := range m.Mentions {
		if id == self {
			return kindMention
		}
	}
	if isHuman(m.Author) && onlyBot(audience, self) {
		return kindMention
	}
	return kindUpdate
}

func isHuman(actorID string) bool {
	kind, _ := chat.ActorName(actorID)
	return kind == chat.KindHuman
}

// onlyBot reports whether self is the single bot among the participants.
func onlyBot(audience []string, self string) bool {
	found := false
	for _, id := range audience {
		if isHuman(id) {
			continue
		}
		if id != self {
			return false
		}
		found = true
	}
	return found
}
