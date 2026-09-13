package uisession

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gigovich/aigem/internal/config"
	"github.com/gigovich/aigem/internal/tools"
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
//	<state>/journal/<session-id>/artifacts.json
//
// The id is checked rather than trusted. No route hands one straight from a URL
// today - a request names a run, and the run record supplies the session id -
// but that record is a JSON file in the state directory, and a single path
// element is the whole of what a session id ever is. Anything else is a way to
// point this function at a directory that is not a journal.
func journalDir(id string) (string, error) {
	if id == "" || id == "." || id == ".." || id != filepath.Base(id) {
		return "", fmt.Errorf("uisession: %q is not a session id", id)
	}
	base, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "journal", id), nil
}

// journalTextCap bounds how much of a tool result is written inline. Anything
// larger is kept beside the journal and fetched when someone expands the call.
// Without the split, one grep over a generated tree lands in the journal and in
// every reconnect after it; the model itself only ever sees a clipped result,
// so the whole body kept beside it is already bounded by the agent.
const journalTextCap = 2048

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

// putBlob stores a tool result too large to journal inline, keyed by the
// event's sequence number. A call id would not do: when a provider supplies
// none the agent numbers them per agent, so two concurrent subagents both
// produce "call-1" - and the seq is unique by construction.
//
// It reports whether the body is now readable, and the caller only marks the
// event as having one when it is: an event promising a body that was never
// written is worse than one that admits it was trimmed.
//
// The write goes through a unique temp name and a rename, so a reader sees
// either nothing or a whole body, and two writers racing the same seq cannot
// splice their output into one file. That race is not supposed to happen - the
// seq is unique and the journal is written under the session's lock - but the
// lock is one process's, and nothing stops two aigem processes resuming one
// session id. A fixed temp name would turn that into a served body belonging to
// neither conversation. It settles the tearing and not the race: the last
// rename still wins, so one of the two events goes on naming a length the file
// it points at no longer has. Only a lock on the journal would settle that, and
// the duplicate sequence numbers underneath it are the older half of the same
// problem.
//
// A process killed between the two steps leaves its temp behind, and nothing
// sweeps them: the name is never served, and a journal is not pruned at all
// today, so this is one small file per abrupt death rather than a class of its
// own.
//
// The bytes are flushed before the rename. Without it a crash can leave the
// name in place over blocks that were never written, and a zero-length file
// would be served as if it were the result - the one failure this route must
// not have, because a client cannot tell a wrong body from a right one. It is
// an fsync on the session's lock, taken for every tool result over the
// threshold; that stalls the conversation's other readers for the length of one
// small write, which is the price of not serving fiction.
//
// The rename is deliberately not made durable in turn. The journal line that
// points at the blob is flushed and not fsynced either, so a directory sync
// here would outlive the record naming it, and losing the rename leaves the
// truncated head - which is what a client renders when there is no blob. That
// asymmetry is also why this is not store.writeAtomically: that one syncs the
// directory and sweeps for orphans on every call, which is the right policy for
// a document read back at startup and the wrong one for a hot path holding a
// session's lock.
func (j *journal) putBlob(seq uint64, body string) bool {
	if j == nil || !j.open {
		return false
	}
	dir := filepath.Join(j.dir, "blobs")
	name := strconv.FormatUint(seq, 10)
	f, err := os.CreateTemp(dir, name+"-*.part")
	if err != nil {
		j.note(err)
		return false
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once the rename has moved it
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		j.note(err)
		return false
	}
	if err := f.Sync(); err != nil {
		f.Close()
		j.note(err)
		return false
	}
	if err := f.Close(); err != nil {
		j.note(err)
		return false
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		j.note(err)
		return false
	}
	return true
}

// putArtifacts replaces the record of what this session changed on disk. It
// is written whole and renamed into place, so a reader never sees half of it.
//
// Unlike putBlob and append, it runs after the journal is closed too: Save is
// called once a turn has fully unwound, which is after Close has already shut
// the events writer down, and the artifacts a session made are as true then as
// they were the moment before. Only a session with no journal at all - one
// that never reached its first turn - has nothing to write beside.
func (j *journal) putArtifacts(arts map[string]tools.FileChange) bool {
	if j == nil {
		return false
	}
	body, err := json.Marshal(arts)
	if err != nil {
		j.note(err)
		return false
	}
	f, err := os.CreateTemp(j.dir, "artifacts-*.part")
	if err != nil {
		j.note(err)
		return false
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(body); err != nil {
		f.Close()
		j.note(err)
		return false
	}
	// Flushed before the rename, as putBlob is: a crash between the two steps
	// must not leave the name in place over a body that was never written.
	if err := f.Sync(); err != nil {
		f.Close()
		j.note(err)
		return false
	}
	if err := f.Close(); err != nil {
		j.note(err)
		return false
	}
	if err := os.Rename(tmp, filepath.Join(j.dir, "artifacts.json")); err != nil {
		j.note(err)
		return false
	}
	return true
}

// ReadArtifacts returns what a session changed on disk, for a session that is
// closed or belongs to a daemon that has since restarted. A session that never
// wrote any is the underlying not-exist error.
func ReadArtifacts(id string) (map[string]tools.FileChange, error) {
	dir, err := journalDir(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "artifacts.json"))
	if err != nil {
		return nil, err
	}
	var out map[string]tools.FileChange
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("uisession: artifacts of %s: %w", id, err)
	}
	return out, nil
}

// ReadBlob returns the whole body of a tool result whose journalled form was
// trimmed, for the session id the journal is keyed by.
//
// It reads the file rather than a live session, so it answers for a closed run
// and for one whose daemon has since restarted, exactly as ReadJournal does. A
// seq with no stored body is the underlying not-exist error: the event either
// said it had one or it did not, and a caller distinguishes the two.
func ReadBlob(id string, seq uint64) (string, error) {
	dir, err := journalDir(id)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, "blobs", strconv.FormatUint(seq, 10)))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadJournal returns the events recorded for a session after since. It is how
// a client that was away longer than the retained history catches up, how a
// resumed conversation gets its timeline back, and how a caller with no live
// session at all reads one - a run whose daemon has since restarted, whose
// conversation is over and whose timeline is only on disk.
//
// A session that never reached its first turn has no journal, and a caller gets
// the underlying not-exist error rather than an empty timeline: "nothing was
// recorded" and "nothing happened" are answers a caller may want to tell apart.
func ReadJournal(id string, since uint64) ([]Event, error) {
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
// written beside the journal and replaced by its head, so the stored timeline
// stays small enough to ship on every reconnect. Live subscribers get the event
// whole; only what is kept is trimmed.
func (l *Local) journalled(ev Event) Event {
	if ev.Kind != KindToolEnd && ev.Kind != KindSubToolEnd {
		return ev
	}
	if len(ev.Text) <= journalTextCap {
		return ev
	}
	stored := ev
	stored.Bytes = len(ev.Text)
	// Blob is set from what the write reported, never ahead of it. A session
	// with no journal - one before its first turn, or one whose state directory
	// could not be written - stores the head and says so.
	stored.Blob = l.journal.putBlob(ev.Seq, ev.Text)
	stored.Text = ev.Text[:journalTextCap]
	return stored
}
