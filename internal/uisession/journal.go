package uisession

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

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
//	<state>/journal/<session-id>/blobs/<seq>
func journalDir(id string) (string, error) {
	base, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "journal", id), nil
}

// blobThreshold bounds how much of a tool result is written inline. Anything
// larger goes beside the journal and is fetched when someone expands the call.
// Without the split, one grep over a generated tree lands in the journal and in
// every reconnect after it; the model itself only ever sees a clipped result,
// so the "full" body here is already bounded by the agent.
const blobThreshold = 2048

// journal appends events for one session and stores oversized tool results
// beside them.
type journal struct {
	dir  string
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
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &journal{dir: dir, f: f, w: bufio.NewWriter(f), open: true}, nil
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

// putBlob stores a tool result too large to journal inline, keyed by the event's
// sequence number. A call id would not do: when a provider supplies none the
// agent numbers them per agent, so two concurrent subagents both produce
// "call-1" - and the seq is unique by construction.
func (j *journal) putBlob(seq uint64, body string) bool {
	if j == nil || !j.open {
		return false
	}
	path := filepath.Join(j.dir, "blobs", strconv.FormatUint(seq, 10))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		j.note(err)
		return false
	}
	return true
}

// Blob returns the stored body of an oversized tool result.
func (l *Local) Blob(seq uint64) (string, error) {
	l.mu.Lock()
	j := l.journal
	l.mu.Unlock()
	if j == nil || !j.open {
		return "", os.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(j.dir, "blobs", strconv.FormatUint(seq, 10)))
	if err != nil {
		return "", err
	}
	return string(b), nil
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
	// A journalled tool result is capped at blobThreshold, but an event carries
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
// written beside the journal and replaced by its head, so the stored timeline
// stays small enough to ship on every reconnect. Live subscribers get the
// event whole; only what is kept is trimmed.
func (l *Local) journalled(ev Event) Event {
	if ev.Kind != KindToolEnd && ev.Kind != KindSubToolEnd {
		return ev
	}
	if len(ev.Text) <= blobThreshold {
		return ev
	}
	stored := ev
	stored.Bytes = len(ev.Text)
	if l.journal.putBlob(ev.Seq, ev.Text) {
		stored.Blob = true
	}
	stored.Text = ev.Text[:blobThreshold]
	return stored
}
