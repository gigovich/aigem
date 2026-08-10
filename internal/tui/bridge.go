package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/gigovich/aigem/internal/uisession"
)

// bridgeEvents translates the session's event stream into the messages this
// model renders, and stops when the model quits.
func bridgeEvents(in <-chan uisession.Event, out chan<- tea.Msg, done <-chan struct{}, sess *uisession.Local) {
	for ev := range in {
		msg := translate(ev, sess)
		if msg == nil {
			continue
		}
		select {
		case out <- msg:
		case <-done:
			return
		}
	}
}

// translate maps one event onto a message, or returns nil for the events this
// front-end has no use for.
func translate(ev uisession.Event, sess *uisession.Local) tea.Msg {
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
		// The event carries only the path: the before and after are held by the
		// session, which keeps the content as it was before this session first
		// touched the file, so a diff shows the session's whole effect.
		c, ok := sess.Artifacts()[ev.Path]
		if !ok {
			return nil
		}
		return fileChangeMsg{path: c.Path, old: c.Old, new: c.New, created: c.Created}
	case uisession.KindTurnEnd:
		return turnDoneMsg{answer: ev.Text, err: turnErr(ev)}
	}
	// user_message and turn_start are the model's own doing - it has already put
	// a richer line in the timeline than the plain text they carry, and a second
	// one would double it. approval_resolved is likewise this front-end's own
	// answer today; it becomes interesting once another client can answer too.
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
