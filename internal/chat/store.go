package chat

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go; see openDB
)

// dsn options, in one place because getting them wrong is quiet rather than
// loud. WAL lets readers run while a write is in flight, foreign_keys makes the
// ON DELETE CASCADE declarations actually do something (SQLite ignores them
// otherwise), and busy_timeout covers the one case a single writer cannot: a
// second process holding the database, which is a misconfiguration worth
// waiting out rather than crashing on.
//
// synchronous(NORMAL) is the one option here with a real tradeoff. Under WAL it
// means a power cut can cost the last commits but cannot corrupt the database.
// That is the right trade for a conversation log: losing the final message of a
// turn the machine did not survive costs one re-ask, while fsyncing every
// commit would put a disk flush inside the writer the whole fleet queues on.
const dsnOptions = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

// maxReaders caps the reader pool. Each SQLite connection carries its own page
// cache, so scaling with NumCPU would cost hundreds of megabytes of idle memory
// on a big host for a daemon whose concurrency is one operator and a handful of
// bots.
const maxReaders = 8

// Store is the conversation store. Every read and write goes through it.
//
// It holds two handles, not one. SQLite serialises writers anyway; making that
// explicit with a single-connection writer removes SQLITE_BUSY from the failure
// set entirely, while readers go wide against the WAL.
type Store struct {
	w   *sql.DB
	r   *sql.DB
	dir string

	// now is injectable so tests can place events in time without sleeping.
	now func() time.Time
	// publish receives the frames a committed write produced. It is set once by
	// the hub before the store serves - a constructor argument would need the
	// hub, which needs the store. It is called synchronously from the write
	// path, so it must not block: a slow subscriber would otherwise stall the
	// fleet's writer.
	publish func([]Frame)
}

// Open opens (and migrates) the store under dir, which holds the database and
// the attachment blobs.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		return nil, fmt.Errorf("chat: create store dir: %w", err)
	}
	path := filepath.Join(dir, "chat.db")

	// Create the file with the mode we want before any connection touches it.
	// SQLite copies the database file's mode onto the -wal and -shm sidecars
	// when it creates them, and those hold the same plaintext - so chmodding
	// only chat.db afterwards protects a third of the data.
	if err := touchPrivate(path); err != nil {
		return nil, err
	}

	w, err := openDB(path, 1)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := securePaths(path, path+"-wal", path+"-shm"); err != nil {
		_ = w.Close()
		return nil, err
	}
	readers := min(max(runtime.NumCPU(), 2), maxReaders)
	r, err := openDB(path, readers)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	return &Store{w: w, r: r, dir: dir, now: time.Now}, nil
}

// touchPrivate creates the file at 0600 if it does not exist, and tightens it
// if it does.
func touchPrivate(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("chat: create the database: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("chat: create the database: %w", err)
	}
	return os.Chmod(path, 0o600)
}

// securePaths tightens whichever of the given files exist. The sidecars are
// absent until the first write and are removed on a clean close, so a missing
// one is normal rather than an error.
func securePaths(paths ...string) error {
	for _, p := range paths {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("chat: secure %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

// openDB opens the database with the pure-Go driver.
//
// modernc.org/sqlite rather than mattn/go-sqlite3 is not a preference: the
// release build sets CGO_ENABLED=0 and cross-compiles six targets, and a cgo
// driver would break every one of them.
func openDB(path string, maxOpen int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?"+dsnOptions)
	if err != nil {
		return nil, fmt.Errorf("chat: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpen)
	// Pinning idle to open keeps the connection - and its page cache - alive
	// between writes. There is no ConnMaxLifetime because there is no server or
	// proxy on the other end to time the connection out.
	db.SetMaxIdleConns(maxOpen)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("chat: open %s: %w", path, err)
	}
	return db, nil
}

// SetPublisher installs the fan-out a committed write feeds. See the field.
func (s *Store) SetPublisher(f func([]Frame)) { s.publish = f }

// Dir is where the store keeps its files.
func (s *Store) Dir() string { return s.dir }

// Close releases both handles.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// write runs fn in a transaction and, once it has committed, publishes whatever
// frames it produced. fn appends to the slice it is given rather than returning
// one, so a write that touches several tables reports each change in the order
// the sequence numbers were handed out.
//
// op names the operation for the error, because a bare driver message like
// "no such column: x" reaching a browser says nothing about where it came from.
func (s *Store) write(ctx context.Context, op string, fn func(tx *sql.Tx, out *[]Frame) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("chat: %s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()

	var frames []Frame
	if err := fn(tx, &frames); err != nil {
		return wrapOp(op, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("chat: %s: %w", op, err)
	}
	if s.publish != nil && len(frames) > 0 {
		s.publish(frames)
	}
	return nil
}

// wrapOp names the operation without burying a sentinel: errors.Is must still
// see ErrNoSuchThread and ErrInvalid through it, and those already read well.
func wrapOp(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNoSuchThread), errors.Is(err, ErrInvalid), errors.Is(err, ErrNoSuchTurn):
		return err
	default:
		return fmt.Errorf("chat: %s: %w", op, err)
	}
}

// nextSeq allocates the next global sequence number. Allocating it inside the
// writing transaction is what lets messages and timeline events share one
// ordering: a reader resuming from a single cursor sees an answer and the work
// that produced it in the order they happened.
func nextSeq(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var seq uint64
	err := tx.QueryRowContext(ctx,
		`UPDATE cursor SET seq = seq + 1 WHERE id = 1 RETURNING seq`).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("allocate sequence: %w", err)
	}
	return seq, nil
}

// Seq is the highest sequence number handed out, which is where a client with
// no history starts following from.
func (s *Store) Seq(ctx context.Context) (uint64, error) {
	var seq uint64
	err := s.r.QueryRowContext(ctx, `SELECT seq FROM cursor WHERE id = 1`).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("chat: read cursor: %w", err)
	}
	return seq, nil
}

// ---- actors ----

// PutActor records or updates an identity. The fleet calls it for every bot at
// startup, so a renamed role or a newly added bot is reflected without a
// migration.
//
// An existing actor keeps its kind. Nothing else in the store would notice a
// human being rewritten into a bot, and the state machine reads that field.
func (s *Store) PutActor(ctx context.Context, a Actor) error {
	if a.ID == "" {
		return invalid("actor id is required")
	}
	kind, name := ActorName(a.ID)
	if kind != KindHuman && kind != KindBot {
		return invalid("actor id %q must start with %q or %q", a.ID, KindHuman+":", KindBot+":")
	}
	a.Kind, a.Name = kind, cmpOr(SanitizeField(a.Name), name)
	a.Role = SanitizeField(a.Role)
	return s.write(ctx, "put actor", func(tx *sql.Tx, _ *[]Frame) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO actors (id, kind, name, role, present, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			   name = excluded.name, role = excluded.role, present = excluded.present`,
			a.ID, a.Kind, a.Name, a.Role, boolInt(a.Present), s.now().UnixMilli())
		return err
	})
}

// SetPresent marks a bot as running, or not.
func (s *Store) SetPresent(ctx context.Context, actorID string, present bool) error {
	return s.write(ctx, "set presence", func(tx *sql.Tx, _ *[]Frame) error {
		_, err := tx.ExecContext(ctx, `UPDATE actors SET present = ? WHERE id = ?`,
			boolInt(present), actorID)
		return err
	})
}

// ClearPresence marks every bot as not running. The fleet calls it at startup:
// a process that was killed had no chance to clear its own flag, so the only
// honest reading of the column after a crash is "unknown", and unknown must not
// render as present.
func (s *Store) ClearPresence(ctx context.Context) error {
	return s.write(ctx, "clear presence", func(tx *sql.Tx, _ *[]Frame) error {
		_, err := tx.ExecContext(ctx, `UPDATE actors SET present = 0 WHERE kind = ?`, KindBot)
		return err
	})
}

// ---- threads ----

// NewThread opens a conversation. Participants are the whole authorization
// boundary, so a thread with none is refused rather than created invisible.
func (s *Store) NewThread(ctx context.Context, title, createdBy string, participants []string) (*Thread, error) {
	if strings.TrimSpace(createdBy) == "" {
		return nil, invalid("a thread needs a creator")
	}
	participants = dedupeActors(append([]string{createdBy}, participants...))
	if len(participants) == 0 {
		return nil, invalid("a thread needs at least one participant")
	}
	if len(participants) > MaxMentions {
		return nil, invalid("a thread may open with at most %d participants", MaxMentions)
	}
	id, err := newThreadID()
	if err != nil {
		return nil, err
	}
	now := s.now()
	th := &Thread{
		ID: id, Title: threadTitle(title), Created: now,
		CreatedBy: createdBy, LastAt: now, State: StateIdle,
	}
	err = s.write(ctx, "new thread", func(tx *sql.Tx, out *[]Frame) error {
		if err := knownActors(ctx, tx, participants); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO threads (id, title, created_at, created_by, last_at, state)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			th.ID, th.Title, now.UnixMilli(), createdBy, now.UnixMilli(), StateIdle); err != nil {
			return err
		}
		for _, actor := range participants {
			if err := addParticipant(ctx, tx, th.ID, actor, createdBy, now); err != nil {
				return err
			}
		}
		// The opening frame goes through publishThread like every other thread
		// change, so a client never has to cope with a second fill level of the
		// same frame type - and so the thread leaves a changed_seq behind for a
		// client that was away when it was created.
		return s.publishThread(ctx, tx, out, th.ID)
	})
	if err != nil {
		return nil, err
	}
	return th, nil
}

// knownActors turns a foreign key violation into an answer the caller can act
// on. The raw constraint error names a column, and - worse - distinguishes a
// real thread from a fake one, which is the distinction ErrNoSuchThread exists
// to hide.
func knownActors(ctx context.Context, tx *sql.Tx, ids []string) error {
	for _, id := range ids {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM actors WHERE id = ?`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return invalid("no such actor %q", id)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// SetTitle renames a thread.
func (s *Store) SetTitle(ctx context.Context, actor, threadID, title string) error {
	return s.mutateThread(ctx, "set title", actor, threadID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE threads SET title = ? WHERE id = ?`,
			threadTitle(title), threadID)
		return err
	})
}

// threadTitle bounds and sanitises a title. It is written by a model and read
// by a model, in a place the reader will take as runtime-authored framing, so
// newlines - which could forge a line that looks like ours - do not survive it.
func threadTitle(title string) string {
	t := SanitizeField(title)
	if r := []rune(t); len(r) > MaxTitleChars {
		t = string(r[:MaxTitleChars])
	}
	return t
}

// SetArchived moves a thread out of the inbox, or back into it.
func (s *Store) SetArchived(ctx context.Context, actor, threadID string, archived bool) error {
	return s.mutateThread(ctx, "archive", actor, threadID, func(tx *sql.Tx) error {
		var at any
		if archived {
			at = s.now().UnixMilli()
		}
		_, err := tx.ExecContext(ctx, `UPDATE threads SET archived_at = ? WHERE id = ?`,
			at, threadID)
		return err
	})
}

// mutateThread applies a change a participant is allowed to make and publishes
// the thread's new shape, so every attached client redraws the row.
func (s *Store) mutateThread(ctx context.Context, op, actor, threadID string, fn func(*sql.Tx) error) error {
	return s.write(ctx, op, func(tx *sql.Tx, out *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, actor); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return s.publishThread(ctx, tx, out, threadID)
	})
}

// DeleteThread destroys a conversation and everything hanging off it.
//
// Only the operator may do it. Deleting is the one irreversible operation here
// and the fleet reads attacker-written pages for a living, so a single injected
// instruction must not be able to destroy the record - which is the whole
// reason the record exists.
func (s *Store) DeleteThread(ctx context.Context, actor, threadID string) error {
	return s.write(ctx, "delete thread", func(tx *sql.Tx, out *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, actor); err != nil {
			return err
		}
		if kindOf(actor) != KindHuman {
			return invalid("only a person may delete a thread")
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		// The tombstone is written before the delete, while the participants
		// still exist to be recorded: they are what tells a client that was away
		// whether this deletion was any of its business.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO deleted_threads (seq, thread_id, actor_id)
			 SELECT ?, ?, actor_id FROM participants WHERE thread_id = ?`,
			seq, threadID, threadID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM threads WHERE id = ?`, threadID); err != nil {
			return err
		}
		// A thread frame with no thread on it is how a client learns the row is
		// gone; there is nothing left to describe.
		*out = append(*out, Frame{Seq: seq, Stream: StreamThread, ThreadID: threadID})
		return nil
	})
}

// ---- participants ----

// AddParticipant pulls an actor into a thread.
//
// Only a current participant may do it: adding someone is a capability, and the
// alternative is any bot pulling any other bot into any conversation. Note that
// it is retroactive - the new participant can read and search the whole
// transcript, including what was said before they arrived. That follows from
// participation being the boundary, and it is why adding is a capability.
func (s *Store) AddParticipant(ctx context.Context, addedBy, threadID, actor string) error {
	return s.write(ctx, "add participant", func(tx *sql.Tx, out *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, addedBy); err != nil {
			return err
		}
		if err := knownActors(ctx, tx, []string{actor}); err != nil {
			return err
		}
		already, err := isParticipant(ctx, tx, threadID, actor)
		if err != nil || already {
			return err
		}
		if err := addParticipant(ctx, tx, threadID, actor, addedBy, s.now()); err != nil {
			return err
		}
		_, name := ActorName(actor)
		return s.systemMessage(ctx, tx, out, threadID, addedBy, name+" joined the thread")
	})
}

// RemoveParticipant drops an actor from a thread, and says so in the
// transcript: a conversation whose membership changed silently cannot be
// audited.
//
// A bot may only remove itself. Letting one participant evict another made
// eviction of the operator a lockout with no way back - rejoining needs a
// participant to do the adding - which is a destructive primitive to leave one
// prompt injection away.
//
// The last participant may not leave. A thread with nobody in it is readable,
// writable and deletable by no one, and NewThread refuses to create that state
// in the first place.
func (s *Store) RemoveParticipant(ctx context.Context, removedBy, threadID, actor string) error {
	return s.write(ctx, "remove participant", func(tx *sql.Tx, out *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, removedBy); err != nil {
			return err
		}
		if removedBy != actor && kindOf(removedBy) != KindHuman {
			return invalid("a bot may only remove itself from a thread")
		}
		var left int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM participants WHERE thread_id = ?`, threadID).Scan(&left); err != nil {
			return err
		}
		if left <= 1 {
			return invalid("a thread cannot be left with no participants")
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM participants WHERE thread_id = ? AND actor_id = ?`, threadID, actor)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return nil
		}
		_, name := ActorName(actor)
		return s.systemMessage(ctx, tx, out, threadID, removedBy, name+" left the thread")
	})
}

// IsParticipant answers, from the one store that decides it, whether an actor is
// in a thread. Mattermost made this a guess: the recipient asked the chat server
// with its own credentials and fell back when it could not confirm. Here there
// is no second authority to disagree with, so a refusal is final.
func (s *Store) IsParticipant(ctx context.Context, threadID, actor string) (bool, error) {
	var one int
	err := s.r.QueryRowContext(ctx,
		`SELECT 1 FROM participants WHERE thread_id = ? AND actor_id = ?`,
		threadID, actor).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("chat: check participation: %w", err)
	}
	return true, nil
}

func addParticipant(ctx context.Context, tx *sql.Tx, threadID, actor, by string, at time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO participants (thread_id, actor_id, added_at, added_by)
		 VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		threadID, actor, at.UnixMilli(), by)
	return err
}

func isParticipant(ctx context.Context, tx *sql.Tx, threadID, actor string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM participants WHERE thread_id = ? AND actor_id = ?`,
		threadID, actor).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func requireParticipant(ctx context.Context, tx *sql.Tx, threadID, actor string) error {
	ok, err := isParticipant(ctx, tx, threadID, actor)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoSuchThread
	}
	return nil
}

// ---- messages ----

// Say writes a message. It is the only way text enters a thread, so the state
// the inbox sorts by is recomputed here and nowhere else.
func (s *Store) Say(ctx context.Context, threadID string, m Draft) (Message, error) {
	body := strings.TrimSpace(m.Body)
	switch {
	case body == "":
		return Message{}, invalid("a message needs a body")
	case len(body) > MaxBodyBytes:
		return Message{}, invalid("a message may be at most %s", HumanSize(MaxBodyBytes))
	case m.Kind != "" && m.Kind != MsgMessage && m.Kind != MsgHandoff:
		// MsgSystem is reserved. Letting a caller pick it would let a bot forge
		// a membership note indistinguishable from one the store wrote, in the
		// transcript that exists to make membership changes auditable.
		return Message{}, invalid("%q is not a message kind a caller may write", m.Kind)
	}
	if m.Kind == "" {
		m.Kind = MsgMessage
	}
	mentions := dedupeActors(m.Mentions)
	attachments := dedupeActors(m.Attachments)
	if len(mentions) > MaxMentions {
		return Message{}, invalid("a message may mention at most %d actors", MaxMentions)
	}
	if len(attachments) > MaxAttachments {
		return Message{}, invalid("a message may carry at most %d attachments", MaxAttachments)
	}
	out := Message{
		Thread: threadID, Author: m.Author, Body: body, Kind: m.Kind,
		Mentions: mentions, Await: m.AwaitReply, Created: s.now(), Attachments: attachments,
	}
	err := s.write(ctx, "say", func(tx *sql.Tx, frames *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, m.Author); err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		out.Seq = seq
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages (seq, thread_id, author_id, body, kind, mentions, await, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			seq, threadID, m.Author, body, m.Kind, padMentions(mentions),
			boolInt(m.AwaitReply), out.Created.UnixMilli()); err != nil {
			return err
		}
		if err := claimAttachments(ctx, tx, threadID, seq, attachments); err != nil {
			return err
		}
		if err := s.noteMessage(ctx, tx, threadID, out); err != nil {
			return err
		}
		msg := out
		*frames = append(*frames, Frame{Seq: seq, Stream: StreamMessage, ThreadID: threadID, Message: &msg})
		return s.publishThread(ctx, tx, frames, threadID)
	})
	if err != nil {
		return Message{}, err
	}
	return out, nil
}

// claimAttachments links uploads to the message carrying them, and refuses an
// id that is not an upload in this thread. Ignoring those silently let a
// published frame advertise a file the stored transcript would never return.
func claimAttachments(ctx context.Context, tx *sql.Tx, threadID string, seq uint64, ids []string) error {
	for _, id := range ids {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO message_attachments (message_seq, attachment_id)
			 SELECT ?, id FROM attachments WHERE id = ? AND thread_id = ?`,
			seq, id, threadID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return invalid("no attachment %q in this thread", id)
		}
	}
	return nil
}

// systemMessage records something the store did rather than someone said. It
// runs inside a caller's transaction, so membership changes and the note about
// them cannot come apart.
func (s *Store) systemMessage(ctx context.Context, tx *sql.Tx, frames *[]Frame,
	threadID, author, body string) error {
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	msg := Message{
		Seq: seq, Thread: threadID, Author: author, Body: body,
		Kind: MsgSystem, Created: s.now(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (seq, thread_id, author_id, body, kind, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		seq, threadID, author, body, MsgSystem, msg.Created.UnixMilli()); err != nil {
		return err
	}
	if err := s.noteMessage(ctx, tx, threadID, msg); err != nil {
		return err
	}
	*frames = append(*frames, Frame{Seq: seq, Stream: StreamMessage, ThreadID: threadID, Message: &msg})
	return s.publishThread(ctx, tx, frames, threadID)
}

// noteMessage updates the denormalised tail and the resting state.
//
// The stored state is only ever a resting one. "working" is not written here
// because it is a fact about the turns table, and a column that duplicated it
// would be wrong for exactly as long as a crash left a turn open.
func (s *Store) noteMessage(ctx context.Context, tx *sql.Tx, threadID string, m Message) error {
	state, err := s.restingState(ctx, tx, threadID, m)
	if err != nil {
		return err
	}
	if state == "" {
		// A membership note answers nothing and asks nothing; leave the state be.
		_, err := tx.ExecContext(ctx,
			`UPDATE threads SET last_seq = ?, last_at = ?, last_author = ?, last_text = ?
			 WHERE id = ?`,
			m.Seq, m.Created.UnixMilli(), m.Author, preview(m.Body), threadID)
		return err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE threads SET last_seq = ?, last_at = ?, last_author = ?, last_text = ?, state = ?
		 WHERE id = ?`,
		m.Seq, m.Created.UnixMilli(), m.Author, preview(m.Body), state, threadID)
	return err
}

// restingState decides what a message leaves the thread in, or "" to leave the
// state alone.
//
// Three rules that are easy to get subtly wrong:
//
//   - Only a bot can put a thread into needs_you. The operator asking a question
//     with AwaitReply set would otherwise demand their own attention.
//   - Only a person can take it back out. A second bot chiming in used to clear
//     the accent while the operator was still owed the answer.
//   - AwaitReply is only honoured when a person is actually in the thread.
//     Otherwise a bot-only conversation parks itself in needs_you forever, where
//     nobody who could answer will ever see it.
func (s *Store) restingState(ctx context.Context, tx *sql.Tx, threadID string, m Message) (string, error) {
	if m.Kind == MsgSystem {
		return "", nil
	}
	if kindOf(m.Author) == KindHuman {
		return StateWaiting, nil
	}
	if m.Await {
		human, err := hasHumanParticipant(ctx, tx, threadID)
		if err != nil {
			return "", err
		}
		if human {
			return StateNeedsYou, nil
		}
	}
	var current string
	err := tx.QueryRowContext(ctx, `SELECT state FROM threads WHERE id = ?`, threadID).Scan(&current)
	if err != nil {
		return "", err
	}
	if current == StateNeedsYou {
		return StateNeedsYou, nil
	}
	return StateIdle, nil
}

func hasHumanParticipant(ctx context.Context, tx *sql.Tx, threadID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM participants p JOIN actors a ON a.id = p.actor_id
		  WHERE p.thread_id = ? AND a.kind = ?`, threadID, KindHuman).Scan(&n)
	return n > 0, err
}

// publishThread records that the thread changed and appends its current shape
// to the frames a write produced, so an inbox row redraws without refetching
// the list.
//
// changed_seq is stamped here rather than at each call site because every
// thread-level change goes through this function, and a change that published
// live without leaving a durable mark would be invisible to a client that was
// away when it happened.
//
// The view is rendered for the operator. Unread is per-reader by definition, and
// the operator is the only reader frames are fanned out to: the browser UI is
// theirs, and bots read the store directly rather than following this stream.
// If a second human is ever added, this is the line that has to change.
func (s *Store) publishThread(ctx context.Context, tx *sql.Tx, frames *[]Frame, threadID string) error {
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET changed_seq = ? WHERE id = ?`,
		seq, threadID); err != nil {
		return err
	}
	view, err := threadViewTx(ctx, tx, Operator, threadID)
	if err != nil {
		return err
	}
	*frames = append(*frames, Frame{Seq: seq, Stream: StreamThread, ThreadID: threadID, Thread: view})
	return nil
}

// MarkRead records how far an actor has read, which is what the inbox counts
// against.
//
// It moves forwards only, and never past what exists. Reading backwards would
// let a client scrolling up resurrect its own badge; accepting a seq from the
// future would let one buggy client silence a thread permanently, since the
// value can never be lowered again.
func (s *Store) MarkRead(ctx context.Context, actor, threadID string, seq uint64) error {
	return s.write(ctx, "mark read", func(tx *sql.Tx, frames *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, actor); err != nil {
			return err
		}
		var last uint64
		if err := tx.QueryRowContext(ctx, `SELECT last_seq FROM threads WHERE id = ?`,
			threadID).Scan(&last); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO read_state (actor_id, thread_id, last_read_seq) VALUES (?, ?, ?)
			 ON CONFLICT(actor_id, thread_id) DO UPDATE SET
			   last_read_seq = excluded.last_read_seq
			 WHERE excluded.last_read_seq > read_state.last_read_seq`,
			actor, threadID, min(seq, last))
		if err != nil {
			return err
		}
		// The UI marks read on scroll. Publishing a frame - three correlated
		// subqueries and a sequence number - for a call that changed nothing is
		// the difference between a cheap operation and a fleet-wide one.
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		return s.publishThread(ctx, tx, frames, threadID)
	})
}

// ---- helpers ----

func newThreadID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("chat: new thread id: %w", err)
	}
	return "t_" + hex.EncodeToString(b), nil
}

// ValidThreadID reports whether id has the shape this store hands out. The
// tools a model drives check it, so an invented id gets told to list first
// rather than a bare "not found" the model will try to work around.
func ValidThreadID(id string) bool {
	rest, ok := strings.CutPrefix(id, "t_")
	return ok && isHex(rest, 16)
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func kindOf(actorID string) string {
	kind, _ := ActorName(actorID)
	return kind
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// dedupeActors keeps the first occurrence of each id and drops the empties, so
// a caller may pass the author twice without opening a thread that lists them
// twice.
func dedupeActors(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// padMentions stores ids with a space on each side, so a LIKE for one id cannot
// match a prefix of another name.
func padMentions(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return " " + strings.Join(ids, " ") + " "
}

func splitMentions(s string) []string { return strings.Fields(s) }
