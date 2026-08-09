package uisession

import (
	"time"

	"github.com/gigovich/aigem/internal/session"
	"github.com/gigovich/aigem/internal/tools"
)

// A session gets its id on its first turn, not when it is created. An empty
// conversation someone opened and walked away from is not worth a file, and the
// id is derived from the moment work actually started.
func (l *Local) beginLocked(title string) {
	if l.id != "" {
		return
	}
	l.start = time.Now()
	l.id = session.NewID(l.start)
	if l.title == "" { // a SessionStart hook may have named it already
		l.title = title
	}
	if l.hooks != nil {
		l.hooks.SetSession(l.id, "")
	}
	l.ag.SetSessionID(l.id)
	// Events emitted before the id existed - a client attaching, its presence -
	// belong to this conversation too, so the journal starts with them.
	if j, err := openJournal(l.id); err == nil {
		l.journal = j
		for _, ev := range l.ring {
			j.append(ev)
		}
	}
	l.emitLocked(l.metaEventLocked())
}

// Meta describes the conversation: its id, its title, and when it started. The
// id is empty until the first turn.
func (l *Local) Meta() session.Meta {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.metaLocked()
}

func (l *Local) metaLocked() session.Meta {
	m := session.Meta{ID: l.id, Title: l.title, Created: l.start}
	if l.modelRef != nil {
		m.Model = l.modelRef()
	}
	return m
}

func (l *Local) metaEventLocked() Event {
	meta := l.metaLocked()
	return Event{Kind: KindSessionMeta, ID: meta.ID, Text: meta.Title, Name: meta.Model}
}

// SetTitle renames the conversation.
func (l *Local) SetTitle(title string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.title = title
	l.emitLocked(l.metaEventLocked())
}

// Save persists the conversation. It is a no-op before the first turn, when
// there is nothing to resume into.
func (l *Local) Save() error {
	l.mu.Lock()
	if l.id == "" || l.ag == nil {
		l.mu.Unlock()
		return nil
	}
	s := &session.Session{Meta: l.metaLocked(), Messages: l.ag.Messages()}
	l.mu.Unlock()
	return session.Save(s, time.Now())
}

// Load restores a saved conversation into this session, replacing whatever it
// held. The stored session is returned so a front-end can rebuild its own view
// of it - the message history is what persists, and the timeline a UI draws is
// reconstructed from it.
func (l *Local) Load(id string) (*session.Session, error) {
	s, err := session.Load(id)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ag == nil {
		return nil, ErrClosed
	}
	l.ag.SetMessages(s.Messages)
	l.ag.SetSessionID(s.ID)
	l.id, l.title, l.start = s.ID, s.Title, s.Created
	l.reopenJournalLocked()
	if l.hooks != nil {
		l.hooks.SetSession(l.id, "")
	}
	l.emitLocked(l.metaEventLocked())
	return s, nil
}

// Reset saves the current conversation and starts an empty one: the agent's
// history, the tool policy, the artifacts and the identity all go. The system
// prompt is reassembled, so edits to the project's instruction files take
// effect without a restart.
func (l *Local) Reset() error {
	err := l.Save()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ag != nil {
		l.ag.Reset()
		if l.rebuildSystem != nil {
			l.ag.SetSystem(l.rebuildSystem())
		}
	}
	l.toolPolicy = map[string]string{}
	l.artifacts = map[string]tools.FileChange{}
	l.journal.close()
	l.journal = nil
	l.id, l.title, l.start = "", "", time.Time{}
	if l.hooks != nil {
		l.hooks.SetSession("", "")
	}
	l.emitLocked(l.metaEventLocked())
	return err
}

// reopenJournalLocked attaches to a resumed conversation's journal and picks the
// sequence up where it left off. Numbering restarts at 1 in every process, so
// appending without this would write a second event under a number the file
// already uses, and a client asking for everything after it would get the wrong
// half of two conversations.
func (l *Local) reopenJournalLocked() {
	l.journal.close()
	l.journal = nil
	// The in-memory history describes the conversation just replaced, so it is
	// dropped rather than spliced onto the one being resumed.
	l.ring = nil
	if prior, err := readJournal(l.id, 0); err == nil && len(prior) > 0 {
		if last := prior[len(prior)-1].Seq; last > l.seq {
			l.seq = last
		}
	}
	if j, err := openJournal(l.id); err == nil {
		l.journal = j
	}
}

// Timeline returns what was recorded for the conversation now loaded, oldest
// first. It is the timeline a front-end draws on resume: the saved messages say
// what the agent remembers, and this says what happened.
func (l *Local) Timeline() ([]Event, error) {
	l.mu.Lock()
	id := l.id
	l.mu.Unlock()
	if id == "" {
		return nil, nil
	}
	return readJournal(id, 0)
}

// SetRebuildSystem installs the system-prompt assembler after construction,
// for a caller that only has it once the session exists.
func (l *Local) SetRebuildSystem(f func() string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rebuildSystem = f
}

// RebuildSystem reassembles the system prompt in place, for a front-end that
// has changed something the prompt is built from without starting over.
func (l *Local) RebuildSystem() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ag != nil && l.rebuildSystem != nil {
		l.ag.SetSystem(l.rebuildSystem())
	}
}
