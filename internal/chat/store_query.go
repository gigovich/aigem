package chat

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// liveTurn is whether a turn is running in this thread right now. "working" is
// a fact about the turns table, so a stored column duplicating it would be
// wrong for exactly as long as a crash left a turn open.
const liveTurn = `EXISTS(SELECT 1 FROM turns tn
	WHERE tn.thread_id = t.id AND tn.ended_at IS NULL)`

// effectiveState is the state a reader sees: the resting state a message left
// behind, unless a turn is running over it. It is one expression, used by the
// projection and by the filter, so what the inbox shows and what it filters on
// cannot drift apart.
const effectiveState = `CASE WHEN ` + liveTurn + ` THEN 'working' ELSE t.state END`

// threadColumns is the thread row every view is built from. It is one string so
// the inbox query and the single-thread query cannot drift apart.
const threadColumns = `t.id, t.title, t.created_at, t.created_by,
	t.last_seq, t.last_at, t.last_author, t.last_text, ` + effectiveState + `, t.archived_at`

// viewExtras are the three things a row shows that are not on the thread: how
// much the reader has not seen, who is in it, and whether a turn is running.
// They are subqueries rather than joins so one statement draws the whole inbox
// without a fan-out per thread.
//
// The unread count is capped. An exact number stops being information long
// before it stops being expensive to compute, and for a reader who has never
// marked anything read the uncapped version counts the thread's entire history
// on every draw.
const viewExtras = `
	(SELECT count(*) FROM (
	   SELECT 1 FROM messages m
	    WHERE m.thread_id = t.id
	      AND m.seq > coalesce(rs.last_read_seq, 0)
	      AND m.author_id <> :me
	    LIMIT ` + maxUnreadSQL + `))                                 AS unread,
	(SELECT group_concat(p2.actor_id, ' ') FROM participants p2
	  WHERE p2.thread_id = t.id)                                    AS actors,
	` + liveTurn + ` AS working`

// MaxUnread bounds the unread count. Past it the UI says "99+".
const MaxUnread = 99

const maxUnreadSQL = "99"

// viewSelect builds the projection over threads for a given reader.
func viewSelect(where string) string {
	return `SELECT ` + threadColumns + `,` + viewExtras + `
		  FROM threads t
		  LEFT JOIN read_state rs ON rs.thread_id = t.id AND rs.actor_id = :me
		 WHERE ` + where
}

// Inbox lists the threads an actor is in, newest activity first.
//
// state filters by the effective state, which is not always the stored one, and
// the filter is in the statement rather than applied to the rows afterwards:
// doing it in Go would let LIMIT drop matching threads to make room for ones
// that were then filtered out, which reads as an inbox that lost a thread.
func (s *Store) Inbox(ctx context.Context, actor, state string, archived bool, limit int) ([]ThreadView, error) {
	limit = clampLimit(limit, 100, 500)
	arch := "t.archived_at IS NULL"
	if archived {
		arch = "t.archived_at IS NOT NULL"
	}
	rows, err := s.r.QueryContext(ctx,
		viewSelect(`t.id IN (SELECT thread_id FROM participants WHERE actor_id = :me)
		   AND `+arch+`
		   AND (:state = '' OR `+effectiveState+` = :state)`)+`
		 ORDER BY t.changed_seq DESC
		 LIMIT :limit`,
		sql.Named("me", actor), sql.Named("state", state), sql.Named("limit", limit))
	if err != nil {
		return nil, fmt.Errorf("chat: read inbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ThreadView{}
	for rows.Next() {
		v, err := scanThreadView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ThreadFor returns one thread as the reader sees it.
func (s *Store) ThreadFor(ctx context.Context, actor, threadID string) (ThreadView, error) {
	row := s.r.QueryRowContext(ctx,
		viewSelect(`t.id = :id
		   AND t.id IN (SELECT thread_id FROM participants WHERE actor_id = :me)`),
		sql.Named("me", actor), sql.Named("id", threadID))
	v, err := scanThreadView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadView{}, ErrNoSuchThread
	}
	if err != nil {
		return ThreadView{}, fmt.Errorf("chat: read thread: %w", err)
	}
	return v, nil
}

// threadViewsFor returns several threads in one statement. Tail used to fetch
// them one at a time, which cost more than the query it followed.
func (s *Store) threadViewsFor(ctx context.Context, actor string, ids []string) ([]ThreadView, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := []any{sql.Named("me", actor)}
	holes := make([]string, len(ids))
	for i, id := range ids {
		name := fmt.Sprintf("id%d", i)
		holes[i] = ":" + name
		args = append(args, sql.Named(name, id))
	}
	rows, err := s.r.QueryContext(ctx,
		viewSelect(`t.id IN (`+strings.Join(holes, ",")+`)
		   AND t.id IN (SELECT thread_id FROM participants WHERE actor_id = :me)`),
		args...)
	if err != nil {
		return nil, fmt.Errorf("chat: read threads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ThreadView
	for rows.Next() {
		v, err := scanThreadView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// threadViewTx is the same read from inside a write, so a frame describes the
// thread as it is at commit rather than as a second connection later finds it.
func threadViewTx(ctx context.Context, tx *sql.Tx, actor, threadID string) (*ThreadView, error) {
	row := tx.QueryRowContext(ctx, viewSelect(`t.id = :id`),
		sql.Named("me", actor), sql.Named("id", threadID))
	v, err := scanThreadView(row)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// scanner is what both QueryRow and Rows satisfy, so one scan serves both.
type scanner interface{ Scan(dest ...any) error }

func scanThreadView(row scanner) (ThreadView, error) {
	var (
		v        ThreadView
		created  int64
		lastAt   int64
		archived sql.NullInt64
		actors   sql.NullString
	)
	if err := row.Scan(&v.ID, &v.Title, &created, &v.CreatedBy,
		&v.LastSeq, &lastAt, &v.LastAuthor, &v.LastText, &v.State, &archived,
		&v.Unread, &actors, &v.Working); err != nil {
		return ThreadView{}, err
	}
	v.Created = time.UnixMilli(created)
	v.LastAt = time.UnixMilli(lastAt)
	v.Archived = archived.Valid
	// group_concat has no defined order, so two identical reads would otherwise
	// disagree about the participant list and the UI would reshuffle the row.
	v.Participants = slices.Sorted(slices.Values(strings.Fields(actors.String)))
	return v, nil
}

// messageColumns is every field a Message carries, so the three queries that
// return one cannot hand back silently different fill levels.
const messageColumns = `m.seq, m.thread_id, m.author_id, m.body, m.kind,
	m.mentions, m.await, m.created_at, m.turn_seq,
	coalesce((SELECT group_concat(ma.attachment_id, ' ') FROM message_attachments ma
	           WHERE ma.message_seq = m.seq), '')`

func scanMessage(row scanner) (Message, error) {
	var (
		m           Message
		mentions    string
		created     int64
		attachments string
	)
	if err := row.Scan(&m.Seq, &m.Thread, &m.Author, &m.Body, &m.Kind,
		&mentions, &m.Await, &created, &m.Turn, &attachments); err != nil {
		return Message{}, err
	}
	m.Mentions = splitMentions(mentions)
	m.Created = time.UnixMilli(created)
	m.Attachments = slices.Sorted(slices.Values(strings.Fields(attachments)))
	return m, nil
}

// Messages returns a page of a thread's messages, newest first, ending just
// below before. A zero before starts at the newest.
//
// cursor is what to pass as the next before, and more says whether older
// messages remain. They are returned rather than inferred for the same reason
// Tail returns them: a caller cannot tell a page that filled its limit from the
// start of the conversation, and one that guesses loses the rest of the thread
// without ever being told it had one.
func (s *Store) Messages(ctx context.Context, actor, threadID string, before uint64, limit int) (
	msgs []Message, cursor uint64, more bool, err error) {
	if err := s.requireParticipantR(ctx, threadID, actor); err != nil {
		return nil, 0, false, err
	}
	limit = clampLimit(limit, 100, 500)
	// One more than asked for, so a full page is distinguishable from the end of
	// the thread without a second query.
	rows, err := s.r.QueryContext(ctx,
		`SELECT `+messageColumns+`
		   FROM messages m
		  WHERE m.thread_id = ? AND m.seq < ?
		  ORDER BY m.seq DESC
		  LIMIT ?`,
		threadID, sqlSeq(before), limit+1)
	if err != nil {
		return nil, 0, false, fmt.Errorf("chat: read messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, 0, false, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	if len(out) > limit {
		out = out[:limit]
		// The page runs newest to oldest, so the oldest delivered message is the
		// bound the next page asks below.
		return out, out[len(out)-1].Seq, true, nil
	}
	return out, 0, false, nil
}

// Timeline returns a thread's agent events after since, oldest first. It is how
// a client that reconnected backfills the thread it is watching, over HTTP
// rather than down the socket, so a large backfill cannot blow the socket's
// queue.
//
// cursor is what to pass as the next since, and more says whether the thread
// continues past this page. A single bot turn can be hundreds of events, so a
// backfill hitting the limit is ordinary rather than exceptional.
// A zero turn is every event in the thread; a turn narrows it to that one run,
// which is what expanding a collapsed trace asks for.
func (s *Store) Timeline(ctx context.Context, actor, threadID string, since, turnSeq uint64,
	limit int) (frames []Frame, cursor uint64, more bool, err error) {
	if err := s.requireParticipantR(ctx, threadID, actor); err != nil {
		return nil, 0, false, err
	}
	limit = clampLimit(limit, 500, 2000)
	turnFilter := ""
	args := []any{threadID, since}
	if turnSeq != 0 {
		turnFilter = " AND turn_seq = ?"
		args = append(args, turnSeq)
	}
	args = append(args, limit+1)
	rows, err := s.r.QueryContext(ctx,
		`SELECT seq, turn_seq, payload FROM events
		  WHERE thread_id = ? AND seq > ?`+turnFilter+`
		  ORDER BY seq LIMIT ?`, args...)
	if err != nil {
		return nil, 0, false, fmt.Errorf("chat: read timeline: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Frame{}
	for rows.Next() {
		f := Frame{Stream: StreamEvent, ThreadID: threadID}
		if err := rows.Scan(&f.Seq, &f.Turn, &f.Event); err != nil {
			return nil, 0, false, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	if len(out) > limit {
		out = out[:limit]
		return out, out[len(out)-1].Seq, true, nil
	}
	return out, 0, false, nil
}

// Tail returns everything an actor is entitled to see after since, oldest
// first, and the cursor to resume from.
//
// The cursor is returned rather than inferred. A client cannot take the highest
// Seq it saw: a page that hit the limit stops mid-stream, and the caller has to
// know that to ask for the rest. When more remains, cursor is the last message
// actually delivered and more is true.
//
// Thread frames only go out on the final page. Emitting them earlier would put
// a frame describing a thread as it is now ahead of messages that had not been
// delivered yet, which is how a client resuming from the wrong number loses the
// backlog it was reconnecting for.
//
// Timeline events are deliberately absent. A fleet mid-turn produces hundreds a
// minute across every thread, and shipping all of them to a client showing a
// list is how the fan-out budget goes; a watched thread backfills through
// Timeline instead.
func (s *Store) Tail(ctx context.Context, actor string, since uint64, limit int) (
	frames []Frame, cursor uint64, more bool, err error) {
	limit = clampLimit(limit, 500, 2000)

	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, false, fmt.Errorf("chat: tail: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Everything below is read at one point in the stream, so the cursor a
	// caller resumes from cannot be ahead of the rows they were given.
	var top uint64
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM cursor WHERE id = 1`).Scan(&top); err != nil {
		return nil, 0, false, fmt.Errorf("chat: tail: %w", err)
	}

	// One more than asked for, so a full page is distinguishable from the end
	// of the stream without a second query.
	msgs, err := tailMessages(ctx, tx, actor, since, top, limit+1)
	if err != nil {
		return nil, 0, false, err
	}
	more = len(msgs) > limit
	if more {
		msgs = msgs[:limit]
	}
	frames = make([]Frame, 0, len(msgs))
	for i := range msgs {
		m := msgs[i]
		frames = append(frames, Frame{
			Seq: m.Seq, Stream: StreamMessage, ThreadID: m.Thread, Message: &m,
		})
	}
	if more {
		return frames, msgs[len(msgs)-1].Seq, true, nil
	}

	changed, err := changedThreads(ctx, tx, actor, since, top)
	if err != nil {
		return nil, 0, false, err
	}
	ids := make([]string, 0, len(changed))
	for _, c := range changed {
		ids = append(ids, c.id)
	}
	views, err := s.threadViewsFor(ctx, actor, ids)
	if err != nil {
		return nil, 0, false, err
	}
	at := make(map[string]uint64, len(changed))
	for _, c := range changed {
		at[c.id] = c.seq
	}
	for i := range views {
		v := views[i]
		// changed_seq, not last_seq: a rename or an archived flag moves the
		// former and not the latter, so a frame stamped with last_seq would sort
		// below the cursor a client is about to resume from, and be dropped.
		frames = append(frames, Frame{
			Seq: at[v.ID], Stream: StreamThread, ThreadID: v.ID, Thread: &v,
		})
	}
	tombs, err := tombstones(ctx, tx, actor, since, top)
	if err != nil {
		return nil, 0, false, err
	}
	frames = append(frames, tombs...)
	slices.SortStableFunc(frames, func(a, b Frame) int { return cmp.Compare(a.Seq, b.Seq) })
	return frames, top, false, nil
}

func tailMessages(ctx context.Context, tx *sql.Tx, actor string, since, top uint64, limit int) ([]Message, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+messageColumns+`
		   FROM messages m
		  WHERE m.seq > ? AND m.seq <= ?
		    AND m.thread_id IN (SELECT thread_id FROM participants WHERE actor_id = ?)
		  ORDER BY m.seq
		  LIMIT ?`, since, top, actor, limit)
	if err != nil {
		return nil, fmt.Errorf("chat: tail messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// changedThreads lists the threads that changed in any way since - a rename, an
// archive, a participant, a turn starting or ending - not only the ones that
// received a message. Without changed_seq every one of those changes published
// live and left nothing behind for a client that was away.
// threadChange is a thread that moved, and the sequence at which it moved.
type threadChange struct {
	id  string
	seq uint64
}

func changedThreads(ctx context.Context, tx *sql.Tx, actor string, since, top uint64) ([]threadChange, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT t.id, t.changed_seq FROM threads t
		   JOIN participants p ON p.thread_id = t.id AND p.actor_id = ?
		  WHERE t.changed_seq > ? AND t.changed_seq <= ?
		  ORDER BY t.changed_seq`, actor, since, top)
	if err != nil {
		return nil, fmt.Errorf("chat: tail threads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []threadChange
	for rows.Next() {
		var c threadChange
		if err := rows.Scan(&c.id, &c.seq); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// tombstones are the deletions the reader was entitled to hear about, so a
// client that slept through one stops rendering a thread that is gone.
func tombstones(ctx context.Context, tx *sql.Tx, actor string, since, top uint64) ([]Frame, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT seq, thread_id FROM deleted_threads
		  WHERE actor_id = ? AND seq > ? AND seq <= ?
		  ORDER BY seq`, actor, since, top)
	if err != nil {
		return nil, fmt.Errorf("chat: tail tombstones: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Frame
	for rows.Next() {
		f := Frame{Stream: StreamThread}
		if err := rows.Scan(&f.Seq, &f.ThreadID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Search runs a full-text query over the messages in the caller's own threads.
//
// It is the replacement for the ambient channel history a bot used to be handed
// unasked. Scoping it to the caller's threads is not a nicety: participation is
// the only authorization boundary left, and a search that crossed it would be
// the one hole in it.
func (s *Store) Search(ctx context.Context, actor, query string, limit int) ([]Message, error) {
	q := ftsQuery(query)
	if q == "" {
		return nil, invalid("search needs a query")
	}
	limit = clampLimit(limit, 50, 200)
	// Ordering by the FTS rowid rather than by m.seq lets FTS5 walk its index
	// backwards and stop at the limit. They are the same number - the index is
	// keyed on messages.seq - but ordering by the joined column materialises and
	// sorts the whole match set first.
	rows, err := s.r.QueryContext(ctx,
		`SELECT `+messageColumns+`
		   FROM messages_fts f
		   JOIN messages m ON m.seq = f.rowid
		  WHERE messages_fts MATCH ?
		    AND m.thread_id IN (SELECT thread_id FROM participants WHERE actor_id = ?)
		  ORDER BY f.rowid DESC
		  LIMIT ?`, q, actor, limit)
	if err != nil {
		return nil, fmt.Errorf("chat: search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ftsQuery makes a caller's words safe for FTS5's query grammar by quoting each
// one, and returns "" when nothing searchable is left.
//
// A model writing `auth.Refresh(` or `NOT` would otherwise get a syntax error,
// or a query meaning something it did not intend. Two forms survive the
// quoting because a model reaching for them means them: a whole input in double
// quotes stays a phrase, and a trailing `*` stays a prefix match. Without that,
// documented FTS syntax returned zero hits - which reads to a bot exactly like
// nothing having been said.
func ftsQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	if inner, ok := strings.CutPrefix(q, `"`); ok {
		if body, ok := strings.CutSuffix(inner, `"`); ok && body != "" {
			return `"` + strings.ReplaceAll(body, `"`, `""`) + `"`
		}
	}
	var quoted []string
	for _, f := range strings.Fields(q) {
		prefix := ""
		if trimmed, ok := strings.CutSuffix(f, "*"); ok && trimmed != "" {
			f, prefix = trimmed, "*"
		}
		if f = strings.ReplaceAll(f, `"`, `""`); f != "" {
			quoted = append(quoted, `"`+f+`"`+prefix)
		}
	}
	return strings.Join(quoted, " ")
}

// Actors lists the identities the store knows. The roster is not secret -
// participation is what is protected, and a bot is told who its teammates are
// by its own prompt anyway.
func (s *Store) Actors(ctx context.Context) ([]Actor, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT id, kind, name, role, present, created_at FROM actors ORDER BY kind, name`)
	if err != nil {
		return nil, fmt.Errorf("chat: read actors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Actor{}
	for rows.Next() {
		var a Actor
		var created int64
		if err := rows.Scan(&a.ID, &a.Kind, &a.Name, &a.Role, &a.Present, &created); err != nil {
			return nil, err
		}
		a.Created = time.UnixMilli(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ThreadsFor lists the thread ids an actor participates in, including archived
// ones: participation does not lapse when a thread leaves the inbox, and a bot
// asked about old work must be able to find it.
func (s *Store) ThreadsFor(ctx context.Context, actor string, limit int) ([]string, error) {
	limit = clampLimit(limit, 200, 1000)
	rows, err := s.r.QueryContext(ctx,
		`SELECT t.id FROM threads t
		   JOIN participants p ON p.thread_id = t.id AND p.actor_id = ?
		  ORDER BY t.changed_seq DESC LIMIT ?`, actor, limit)
	if err != nil {
		return nil, fmt.Errorf("chat: read threads for %s: %w", actor, err)
	}
	defer func() { _ = rows.Close() }()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) requireParticipantR(ctx context.Context, threadID, actor string) error {
	ok, err := s.IsParticipant(ctx, threadID, actor)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoSuchThread
	}
	return nil
}

// clampLimit bounds a caller's page size. Out-of-range asks are clamped to the
// nearest legal value rather than reset to the default: asking for one more
// than the maximum and silently getting the default instead is the kind of
// surprise a caller discovers in production.
func clampLimit(limit, def, maxLimit int) int {
	switch {
	case limit <= 0:
		return def
	case limit > maxLimit:
		return maxLimit
	default:
		return limit
	}
}

// sqlSeq maps a zero "from the newest" sentinel onto the largest value SQLite's
// signed INTEGER holds, and clamps anything above it: the driver refuses a
// uint64 with the high bit set, and a caller should not meet that error.
func sqlSeq(seq uint64) int64 {
	const maxInt64 = uint64(1)<<63 - 1
	if seq == 0 || seq > maxInt64 {
		return int64(maxInt64)
	}
	return int64(seq)
}
