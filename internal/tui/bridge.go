package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/gigovich/aigem/internal/uisession"
)

// bridgeEvents follows the session, translating its events into the messages
// this model renders, and resubscribing when it is dropped for falling behind.
//
// Resubscribing is the point. A subscriber that cannot keep up - a burst of tool
// output against a blocked render - is dropped with a marker saying where to
// resume. Without acting on it the terminal stops updating permanently and
// silently: the spinner keeps turning, keys still work, and no event ever
// arrives again.
func bridgeEvents(sess *uisession.Local, client uisession.Client, out chan<- tea.Msg,
	done <-chan struct{}) {
	var since uint64
	for {
		in, detach, err := sess.Subscribe(client, since)
		if err != nil {
			select {
			case out <- noticeMsg("lost the session: " + err.Error()):
			case <-done:
			}
			return
		}
		resume, quit := pump(in, out, done)
		detach()
		if quit {
			return
		}
		since = resume
	}
}

// pump forwards one subscription until it ends. It reports where to resume from
// after a desync, and whether the model is going away.
func pump(in <-chan uisession.Event, out chan<- tea.Msg, done <-chan struct{}) (uint64, bool) {
	var resume uint64
	for ev := range in {
		if ev.Kind == uisession.KindDesync {
			return ev.From, false
		}
		resume = ev.Seq
		msg := translate(ev)
		if msg == nil {
			continue
		}
		select {
		case out <- msg:
		case <-done:
			return resume, true
		}
	}
	// The channel closed without a marker: the session is over.
	return resume, true
}

// translate maps one event onto a message, or returns nil for the events this
// front-end has no use for.
func translate(ev uisession.Event) tea.Msg {
	switch ev.Kind {
	case uisession.KindContent:
		return contentMsg(ev.Text)
	case uisession.KindReasoning:
		return reasoningMsg(ev.Text)
	case uisession.KindAssistantMessage:
		return assistantStepMsg(ev.Text)
	case uisession.KindNotice:
		return noticeMsg(ev.Text)
	case uisession.KindUsage:
		return usageMsg(ev.Tokens)
	case uisession.KindSessionMeta:
		// The gauge's denominator arrives with the conversation's identity, so a
		// model switch updates it the same way whether the session is here or in
		// a daemon.
		return sessionMetaMsg{ctx: ev.Ctx, model: ev.Name}
	case uisession.KindTodo:
		return todoUpdateMsg(ev.Todos)
	case uisession.KindToolStart:
		return toolStartMsg{name: ev.Name, args: string(ev.Args)}
	case uisession.KindToolEnd:
		return toolEndMsg{name: ev.Name, result: ev.Text, err: eventErr(ev)}
	case uisession.KindAgentStart:
		return agentStartMsg{id: ev.ID, agent: ev.Agent, prompt: ev.Text}
	case uisession.KindAgentEnd:
		return agentEndMsg{id: ev.ID, result: ev.Text, err: eventErr(ev)}
	case uisession.KindSubToolStart:
		return subToolStartMsg{id: ev.RunID, agent: ev.Agent, name: ev.Name, args: string(ev.Args)}
	case uisession.KindSubToolEnd:
		return subToolEndMsg{id: ev.RunID, agent: ev.Agent, name: ev.Name,
			result: ev.Text, err: eventErr(ev)}
	case uisession.KindSubNotice:
		return subNoticeMsg{id: ev.RunID, agent: ev.Agent, text: ev.Text}
	case uisession.KindApprovalRequest:
		if ev.Approval == nil {
			return nil
		}
		return confirmReqMsg{id: ev.ID, req: *ev.Approval}
	case uisession.KindFileChanged:
		return fileChangeMsg{path: ev.Path, created: ev.Created}
	case uisession.KindApprovalResolved:
		// Somebody else may have answered: the dialog on this screen is about a
		// request that no longer exists.
		return approvalResolvedMsg{id: ev.ID, by: ev.By, decision: string(ev.Decision)}
	case uisession.KindBudgetExhausted:
		return noticeMsg(ev.Text)
	case uisession.KindTurnEnd:
		return turnDoneMsg{answer: ev.Text, err: turnErr(ev)}
	}
	// user_message and turn_start are the model's own doing - it has already put
	// a richer line in the timeline than the plain text they carry, and a second
	// one would double it. tool_batch says which calls were issued together,
	// which this front-end renders as consecutive lines anyway.
	return nil
}

func eventErr(ev uisession.Event) error {
	if ev.Error == "" {
		return nil
	}
	return errString(ev.Error)
}

// turnErr rebuilds the error finishTurn branches on. An interrupted turn comes
// back as the cancellation it really was, so the "interrupted" line is chosen by
// the same test as before rather than by a second, parallel one.
func turnErr(ev uisession.Event) error {
	if ev.Interrupted {
		return context.Canceled
	}
	return eventErr(ev)
}

// errString carries an error that has already been rendered to text by the
// session. Nothing inspects it beyond its message.
type errString string

func (e errString) Error() string { return string(e) }
