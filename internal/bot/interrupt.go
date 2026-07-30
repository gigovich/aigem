package bot

import "strings"

// A turn holds its thread's single-flight lock for as long as it runs - up to the
// role's whole work budget, tens of minutes for a developer. Every message
// addressed to the bot in that window used to wait behind it. On 2026-07-27 a
// "stop, AIGEM is on hold" landed 35 minutes late: the bot kept implementing,
// pushed a branch and asked another bot for QA before it ever saw the message.
//
// So an addressed message is delivered into the running turn instead of queueing
// behind it. What it means is the model's decision, not the runtime's: matching
// stop-words would need a list per language and per phrasing, and would still be
// wrong for "leave #5 alone for now". The model already has the conversation and
// the work in front of it, so it is the thing best placed to judge whether the
// message stops the work, changes it, or means nothing at all.

// Injector is a Runner whose in-flight turn can accept a message. *agent.Agent
// satisfies it.
type Injector interface {
	Inject(text string) bool
}

// midTurnDelivery wraps an inbound message for the model that is mid-turn. It
// says who spoke and demands a judgement, because the default failure is the
// model finishing what it was doing and only then reading it.
func midTurnDelivery(author, text string) string {
	var b strings.Builder
	b.WriteString("--- A message just arrived in this thread while you are working")
	if author != "" {
		b.WriteString(", from ")
		b.WriteString(author)
	}
	b.WriteString(" ---\n\n")
	b.WriteString(text)
	b.WriteString("\n\n--- End of message ---\n\n")
	b.WriteString("Read it before you do anything else. If it tells you to stop, pause, or drop " +
		"this work, comply now: stop immediately, leave what you have done in place without " +
		"pushing, merging, closing, or handing it to anyone, and make your final answer a short " +
		"report of where you stopped. If it changes the work, adjust to it. If it needs nothing " +
		"from you, carry on with what you were doing.")
	return b.String()
}
