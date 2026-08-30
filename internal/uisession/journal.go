package uisession

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/gigovich/aigem/internal/config"
)

// The journal is what a front-end reconnects into. It is kept apart from the
// saved conversation rather than inside it, because the two answer different
// questions: session.Session holds the messages the agent resumes from, and
// compaction evicts messages that a timeline should still show. Storing the
// timeline as events means a reconnecting client renders what happened, not a
// reconstruction of what is left in context.
//
//	<state>/journal/<session-id>/events.jsonl
func journalDir(id string) (string, error) {
	base, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "journal", id), nil
}

// journalTextCap bounds how much of a tool result is written to the journal.
// Without it, one grep over a generated tree lands in the journal and in every
// reconnect after it; the model itself only ever sees a clipped result, so the
// text kept here is already bounded by the agent.
const journalTextCap = 2048

// journal appends events for one session.
type journal struct {
	f    *os.File
	w    *bufio.Writer
	err  error // first write error; reported once and then remembered
	open bool
}

// openJournal starts (or reopens) the journal for a session id. A session that
// cannot write one keeps running: the in-memory history still serves a
// reconnect within this process, and losing the ability to replay across a
// restart is not a reason to refuse to work.
func openJournal(id string) (*journal, error) {
	dir, err := journalDir(id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &journal{f: f, w: bufio.NewWriter(f), open: true}, nil
}

// append writes one event. Records are flushed as they are written: a crash
// mid-turn should cost the last event, not the last thousand.
func (j *journal) append(ev Event) {
	if j == nil || !j.open {
		return
	}
	line, err := json.Marshal(ev)
	if err != nil {
		j.note(err)
		return
	}
	if _, err := j.w.Write(append(line, '\n')); err != nil {
		j.note(err)
		return
	}
	j.note(j.w.Flush())
}

func (j *journal) note(err error) {
	if err != nil && j.err == nil {
		j.err = err
	}
}

func (j *journal) close() {
	if j == nil || !j.open {
		return
	}
	j.note(j.w.Flush())
	j.note(j.f.Close())
	j.open = false
}

// readJournal returns the events recorded for a session after since. It is how
// a client that was away longer than the retained history catches up, and how a
// resumed conversation gets its timeline back.
func readJournal(id string, since uint64) ([]Event, error) {
	dir, err := journalDir(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	// A journalled tool result is capped at journalTextCap, but an event carries
	// other text too (an answer, a delegated prompt), so allow a generous line.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// A torn last line from a crash mid-write is not a reason to refuse the
			// rest of the timeline.
			continue
		}
		if ev.Seq > since {
			out = append(out, ev)
		}
	}
	return out, sc.Err()
}

// journalled prepares an event for storage. A tool result over the threshold is
// truncated to its head, so the stored timeline stays small enough to ship on
// every reconnect. Live subscribers get the event whole; only what is kept is
// trimmed.
func (l *Local) journalled(ev Event) Event {
	if ev.Kind != KindToolEnd && ev.Kind != KindSubToolEnd {
		return ev
	}
	if len(ev.Text) <= journalTextCap {
		return ev
	}
	stored := ev
	stored.Bytes = len(ev.Text)
	stored.Text = ev.Text[:journalTextCap]
	return stored
}
