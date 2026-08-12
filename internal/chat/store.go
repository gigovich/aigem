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

	_ "modernc.org/sqlite" // the pure-Go driver; see openDB for why it must be
)

// dsn options, in one place because getting them wrong is quiet rather than
// loud. WAL lets readers run while a write is in flight, foreign_keys makes the
// ON DELETE CASCADE declarations actually do something (SQLite ignores them
// otherwise), and busy_timeout covers the one case a single writer cannot: a
// second process holding the database, which is a misconfiguration worth
// waiting out rather than crashing on.
const dsnOptions = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

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
	// publish receives the frames a committed write produced. It is set by the
	// hub; until then a write simply produces no fan-out. Publishing after the
	// commit is not a detail: inside it, a subscriber could read a row that then
	// rolls back.
	publish func([]Frame)
}

// Open opens (and migrates) the store under dir, which holds the database and
// the attachment blobs.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		return nil, fmt.Errorf("chat: create store dir: %w", err)
	}
	path := filepath.Join(dir, "chat.db")

	w, err := openDB(path, 1)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}
	// The database file is created by the driver with the process umask, and it
	// holds every conversation the operator has had with the fleet.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("chat: secure the database: %w", err)
	}
	readers := runtime.NumCPU()
	if readers < 2 {
		readers = 2
	}
	r, err := openDB(path, readers)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	return &Store{w: w, r: r, dir: dir, now: time.Now}, nil
}

func openDB(path string, maxOpen int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?"+dsnOptions)
	if err != nil {
		return nil, fmt.Errorf("chat: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("chat: open %s: %w", path, err)
	}
	return db, nil
}

// SetPublisher installs the fan-out a committed write feeds. It is set once,
// before the store is serving.
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
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx, out *[]Frame) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var frames []Frame
	if err := fn(tx, &frames); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.publish != nil && len(frames) > 0 {
		s.publish(frames)
	}
	return nil
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
		return 0, fmt.Errorf("chat: allocate sequence: %w", err)
	}
	return seq, nil
}

// Seq is the highest sequence number handed out, which is where a client with
// no history starts following from.
func (s *Store) Seq(ctx context.Context) (uint64, error) {
	var seq uint64
	err := s.r.QueryRowContext(ctx, `SELECT seq FROM cursor WHERE id = 1`).Scan(&seq)
	return seq, err
}

// ---- actors ----

// PutActor records or updates an identity. The fleet calls it for every bot at
// startup, so a renamed role or a newly added bot is reflected without a
// migration.
func (s *Store) PutActor(ctx context.Context, a Actor) error {
	if a.ID == "" {
		return errors.New("chat: actor id is required")
	}
	kind, name := ActorName(a.ID)
	if a.Kind == "" {
		a.Kind = kind
	}
	if a.Name == "" {
		a.Name = name
	}
	return s.write(ctx, func(tx *sql.Tx, _ *[]Frame) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO actors (id, kind, name, role, present, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			   kind = excluded.kind, name = excluded.name,
			   role = excluded.role, present = excluded.present`,
			a.ID, a.Kind, a.Name, a.Role, boolInt(a.Present), s.now().UnixMilli())
		return err
	})
}

// SetPresent marks a bot as running, or not.
func (s *Store) SetPresent(ctx context.Context, actorID string, present bool) error {
	return s.write(ctx, func(tx *sql.Tx, _ *[]Frame) error {
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
	return s.write(ctx, func(tx *sql.Tx, _ *[]Frame) error {
		_, err := tx.ExecContext(ctx, `UPDATE actors SET present = 0 WHERE kind = ?`, KindBot)
		return err
	})
}

// ---- threads ----

// NewThread opens a conversation. Participants are the whole authorization
// boundary, so a thread with none is refused rather than created invisible.
func (s *Store) NewThread(ctx context.Context, title, createdBy string, participants []string) (*Thread, error) {
	participants = dedupeActors(append([]string{createdBy}, participants...))
	if len(participants) == 0 {
		return nil, errors.New("chat: a thread needs at least one participant")
	}
	id, err := newThreadID()
	if err != nil {
		return nil, err
	}
	now := s.now()
	th := &Thread{
		ID: id, Title: strings.TrimSpace(title), Created: now,
		CreatedBy: createdBy, LastAt: now, State: StateIdle,
	}
	err = s.write(ctx, func(tx *sql.Tx, out *[]Frame) error {
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
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		th.LastSeq = seq
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_seq = ? WHERE id = ?`,
			seq, th.ID); err != nil {
			return err
		}
		view := &ThreadView{Thread: *th, Participants: participants}
		*out = append(*out, Frame{Seq: seq, Stream: StreamThread, Thread: th.ID, Thr: view})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return th, nil
}

// SetTitle renames a thread.
func (s *Store) SetTitle(ctx context.Context, actor, threadID, title string) error {
	return s.mutateThread(ctx, actor, threadID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE threads SET title = ? WHERE id = ?`,
			strings.TrimSpace(title), threadID)
		return err
	})
}

// SetArchived moves a thread out of the inbox, or back into it.
func (s *Store) SetArchived(ctx context.Context, actor, threadID string, archived bool) error {
	return s.mutateThread(ctx, actor, threadID, func(tx *sql.Tx) error {
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
func (s *Store) mutateThread(ctx context.Context, actor, threadID string, fn func(*sql.Tx) error) error {
	return s.write(ctx, func(tx *sql.Tx, out *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, actor); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		view, err := threadViewTx(ctx, tx, actor, threadID)
		if err != nil {
			return err
		}
		*out = append(*out, Frame{Seq: seq, Stream: StreamThread, Thread: threadID, Thr: view})
		return nil
	})
}

// DeleteThread removes a conversation and everything hanging off it.
func (s *Store) DeleteThread(ctx context.Context, actor, threadID string) error {
	return s.write(ctx, func(tx *sql.Tx, out *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, actor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM threads WHERE id = ?`, threadID); err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		// A thread frame with no thread on it is how a client learns the row is
		// gone; there is nothing left to describe.
		*out = append(*out, Frame{Seq: seq, Stream: StreamThread, Thread: threadID})
		return nil
	})
}

// ---- participants ----

// Join adds an actor to a thread. Only a current participant may do it: adding
// someone is a capability, and the alternative is any bot pulling any other bot
// into any conversation.
func (s *Store) Join(ctx context.Context, threadID, actor, addedBy string) error {
	return s.write(ctx, func(tx *sql.Tx, out *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, addedBy); err != nil {
			return err
		}
		already, err := isParticipant(ctx, tx, threadID, actor)
		if err != nil {
			return err
		}
		if already {
			return nil
		}
		now := s.now()
		if err := addParticipant(ctx, tx, threadID, actor, addedBy, now); err != nil {
			return err
		}
		_, name := ActorName(actor)
		return s.systemMessage(ctx, tx, out, threadID, addedBy, name+" joined the thread")
	})
}

// Leave removes an actor from a thread, and says so in the transcript: a
// conversation whose membership changed silently cannot be audited.
func (s *Store) Leave(ctx context.Context, threadID, actor, removedBy string) error {
	return s.write(ctx, func(tx *sql.Tx, out *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, removedBy); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM participants WHERE thread_id = ? AND actor_id = ?`, threadID, actor)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
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
	return err == nil, err
}

func addParticipant(ctx context.Context, tx *sql.Tx, threadID, actor, by string, at time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO participants (thread_id, actor_id, added_at, added_by)
		 VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		threadID, actor, at.UnixMilli(), by)
	if err != nil {
		return fmt.Errorf("chat: add %s to %s: %w", actor, threadID, err)
	}
	return nil
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
func (s *Store) Say(ctx context.Context, threadID string, m Say) (Message, error) {
	body := strings.TrimSpace(m.Body)
	if body == "" {
		return Message{}, errors.New("chat: a message needs a body")
	}
	if m.Kind == "" {
		m.Kind = MsgMessage
	}
	out := Message{
		Thread: threadID, Author: m.Author, Body: body, Kind: m.Kind,
		Mentions: dedupeActors(m.Mentions), Await: m.AwaitReply,
		Created: s.now(), Attachments: m.Attachments,
	}
	err := s.write(ctx, func(tx *sql.Tx, frames *[]Frame) error {
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
			seq, threadID, m.Author, body, m.Kind, padMentions(out.Mentions),
			boolInt(m.AwaitReply), out.Created.UnixMilli()); err != nil {
			return err
		}
		for _, id := range m.Attachments {
			if _, err := tx.ExecContext(ctx,
				`UPDATE attachments SET message_seq = ? WHERE id = ? AND thread_id = ?`,
				seq, id, threadID); err != nil {
				return err
			}
		}
		if err := s.noteMessage(ctx, tx, threadID, out); err != nil {
			return err
		}
		msg := out
		*frames = append(*frames, Frame{Seq: seq, Stream: StreamMessage, Thread: threadID, Msg: &msg})
		return s.publishThread(ctx, tx, frames, threadID)
	})
	if err != nil {
		return Message{}, err
	}
	return out, nil
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
	*frames = append(*frames, Frame{Seq: seq, Stream: StreamMessage, Thread: threadID, Msg: &msg})
	return s.publishThread(ctx, tx, frames, threadID)
}

// noteMessage updates the denormalised tail and the resting state.
//
// The stored state is only ever a resting one. "working" is not written here
// because it is a fact about the turns table, and a column that duplicates it
// would be wrong for exactly as long as a crash left a turn open.
func (s *Store) noteMessage(ctx context.Context, tx *sql.Tx, threadID string, m Message) error {
	state := StateIdle
	switch {
	case m.Kind == MsgSystem:
		// A membership note answers nothing and asks nothing; leave the state be.
		_, err := tx.ExecContext(ctx,
			`UPDATE threads SET last_seq = ?, last_at = ?, last_author = ?, last_text = ?
			 WHERE id = ?`,
			m.Seq, m.Created.UnixMilli(), m.Author, preview(m.Body), threadID)
		return err
	case m.Await:
		state = StateNeedsYou
	case kindOf(m.Author) == KindHuman:
		state = StateWaiting
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE threads SET last_seq = ?, last_at = ?, last_author = ?, last_text = ?, state = ?
		 WHERE id = ?`,
		m.Seq, m.Created.UnixMilli(), m.Author, preview(m.Body), state, threadID)
	return err
}

// publishThread appends the thread's current shape to the frames a write
// produced, so an inbox row redraws without refetching the list.
func (s *Store) publishThread(ctx context.Context, tx *sql.Tx, frames *[]Frame, threadID string) error {
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	view, err := threadViewTx(ctx, tx, Operator, threadID)
	if err != nil {
		return err
	}
	*frames = append(*frames, Frame{Seq: seq, Stream: StreamThread, Thread: threadID, Thr: view})
	return nil
}

// MarkRead records how far an actor has read, which is what the inbox counts
// against. Reading backwards is ignored: a client scrolling up must not undo
// what it has already seen.
func (s *Store) MarkRead(ctx context.Context, actor, threadID string, seq uint64) error {
	return s.write(ctx, func(tx *sql.Tx, frames *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, actor); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO read_state (actor_id, thread_id, last_read_seq) VALUES (?, ?, ?)
			 ON CONFLICT(actor_id, thread_id) DO UPDATE SET
			   last_read_seq = max(last_read_seq, excluded.last_read_seq)`,
			actor, threadID, seq); err != nil {
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

// ValidThreadID reports whether id has the shape this store hands out. Tools
// check it so a model that invented an id gets told to list first, rather than
// a bare "not found" it will try to work around.
func ValidThreadID(id string) bool {
	rest, ok := strings.CutPrefix(id, "t_")
	if !ok || len(rest) != 16 {
		return false
	}
	_, err := hex.DecodeString(rest)
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

func splitMentions(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
}
