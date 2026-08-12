package chat

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// liveTurn is whether a turn is running in this thread right now. "working" is
// a fact about the turns table, so a stored column duplicating it would be
// wrong for exactly as long as a crash left a turn open.
const liveTurn = `EXISTS(SELECT 1 FROM turns tn
	WHERE tn.thread_id = t.id AND tn.ended_at IS NULL)`

// effectiveState is the state a reader sees: the resting state a message left
// behind, unless a turn is running over it. It is one expression, used by every
// query and by the filter, so what the inbox shows and what it filters on
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
const viewExtras = `
	(SELECT count(*) FROM messages m
	  WHERE m.thread_id = t.id
	    AND m.seq > coalesce(rs.last_read_seq, 0)
	    AND m.author_id <> :me)                                    AS unread,
	(SELECT group_concat(p2.actor_id, ' ') FROM participants p2
	  WHERE p2.thread_id = t.id)                                   AS actors,
	` + liveTurn + ` AS working`

// Inbox lists the threads an actor is in, newest activity first.
//
// state filters by the effective state, which is not always the stored one:
// "working" is a fact about the turns table, so a column holding it would be
// wrong for exactly as long as a crash left a turn open.
func (s *Store) Inbox(ctx context.Context, actor, state string, archived bool, limit int) ([]ThreadView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	arch := "t.archived_at IS NULL"
	if archived {
		arch = "t.archived_at IS NOT NULL"
	}
	// The filter is in the statement, not applied to the rows afterwards: doing
	// it in Go would let LIMIT drop matching threads to make room for ones that
	// were then filtered out, which reads as an inbox that lost a thread.
	rows, err := s.r.QueryContext(ctx, `
		SELECT `+threadColumns+`,`+viewExtras+`
		  FROM threads t
		  JOIN participants p ON p.thread_id = t.id AND p.actor_id = :me
		  LEFT JOIN read_state rs ON rs.thread_id = t.id AND rs.actor_id = :me
		 WHERE `+arch+`
		   AND (:state = '' OR `+effectiveState+` = :state)
		 ORDER BY t.last_seq DESC
		 LIMIT :limit`,
		sql.Named("me", actor), sql.Named("state", state), sql.Named("limit", limit))
	if err != nil {
		return nil, err
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
	row := s.r.QueryRowContext(ctx, `
		SELECT `+threadColumns+`,`+viewExtras+`
		  FROM threads t
		  JOIN participants p ON p.thread_id = t.id AND p.actor_id = :me
		  LEFT JOIN read_state rs ON rs.thread_id = t.id AND rs.actor_id = :me
		 WHERE t.id = :id`,
		sql.Named("me", actor), sql.Named("id", threadID))
	v, err := scanThreadView(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadView{}, ErrNoSuchThread
	}
	return v, err
}

// threadViewTx is the same read from inside a write, so a frame describes the
// thread as it is at commit rather than as a second connection later finds it.
func threadViewTx(ctx context.Context, tx *sql.Tx, actor, threadID string) (*ThreadView, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT `+threadColumns+`,`+viewExtras+`
		  FROM threads t
		  LEFT JOIN read_state rs ON rs.thread_id = t.id AND rs.actor_id = :me
		 WHERE t.id = :id`,
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
		working  bool
	)
	if err := row.Scan(&v.ID, &v.Title, &created, &v.CreatedBy,
		&v.LastSeq, &lastAt, &v.LastAuthor, &v.LastText, &v.State, &archived,
		&v.Unread, &actors, &working); err != nil {
		return ThreadView{}, err
	}
	v.Created = time.UnixMilli(created)
	v.LastAt = time.UnixMilli(lastAt)
	v.Archived = archived.Valid
	v.Participants = strings.Fields(actors.String)
	v.Working = working
	return v, nil
}

// Messages returns a page of a thread's messages, newest first, ending just
// below before. A zero before starts at the newest.
func (s *Store) Messages(ctx context.Context, actor, threadID string, before uint64, limit int) ([]Message, error) {
	if err := s.requireParticipantR(ctx, threadID, actor); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if before == 0 {
		before = ^uint64(0) >> 1 // the largest value SQLite's signed INTEGER holds
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT m.seq, m.thread_id, m.author_id, m.body, m.kind, m.mentions, m.await, m.created_at,
		        coalesce(group_concat(a.id, ' '), '')
		   FROM messages m
		   LEFT JOIN attachments a ON a.message_seq = m.seq
		  WHERE m.thread_id = ? AND m.seq < ?
		  GROUP BY m.seq
		  ORDER BY m.seq DESC
		  LIMIT ?`,
		threadID, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []Message{}
	for rows.Next() {
		var (
			m           Message
			mentions    string
			created     int64
			await       bool
			attachments string
		)
		if err := rows.Scan(&m.Seq, &m.Thread, &m.Author, &m.Body, &m.Kind,
			&mentions, &await, &created, &attachments); err != nil {
			return nil, err
		}
		m.Mentions = splitMentions(mentions)
		m.Await = await
		m.Created = time.UnixMilli(created)
		m.Attachments = strings.Fields(attachments)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Timeline returns a thread's agent events after since, oldest first. It is how
// a client that reconnected backfills the thread it is watching, over HTTP
// rather than down the socket, so a large backfill cannot blow the socket's
// queue.
func (s *Store) Timeline(ctx context.Context, actor, threadID string, since uint64, limit int) ([]Frame, error) {
	if err := s.requireParticipantR(ctx, threadID, actor); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT seq, payload FROM events
		  WHERE thread_id = ? AND seq > ?
		  ORDER BY seq LIMIT ?`, threadID, since, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []Frame{}
	for rows.Next() {
		f := Frame{Stream: StreamEvent, Thread: threadID}
		if err := rows.Scan(&f.Seq, &f.Event); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Tail returns everything an actor is entitled to see after since, oldest
// first: the one query a reconnecting client replays to make its inbox correct
// again after a phone slept.
//
// Timeline events are deliberately not in it. A fleet mid-turn produces
// hundreds a minute across every thread, and shipping all of them to a client
// showing a list is how the fan-out budget goes; a watched thread backfills
// through Timeline instead.
func (s *Store) Tail(ctx context.Context, actor string, since uint64, limit int) ([]Frame, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.r.QueryContext(ctx, `
		SELECT m.seq, m.thread_id, m.author_id, m.body, m.kind, m.mentions, m.await, m.created_at
		  FROM messages m
		  JOIN participants p ON p.thread_id = m.thread_id AND p.actor_id = ?
		 WHERE m.seq > ?
		 ORDER BY m.seq
		 LIMIT ?`, actor, since, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []Frame{}
	touched := map[string]bool{}
	for rows.Next() {
		var (
			m        Message
			mentions string
			created  int64
			await    bool
		)
		if err := rows.Scan(&m.Seq, &m.Thread, &m.Author, &m.Body, &m.Kind,
			&mentions, &await, &created); err != nil {
			return nil, err
		}
		m.Mentions = splitMentions(mentions)
		m.Await = await
		m.Created = time.UnixMilli(created)
		msg := m
		out = append(out, Frame{Seq: m.Seq, Stream: StreamMessage, Thread: m.Thread, Msg: &msg})
		touched[m.Thread] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// One current thread frame per thread that moved, rather than a replay of
	// every intermediate shape: the client needs the row as it is now, and the
	// states it passed through while nobody was attached are not information.
	for id := range touched {
		v, err := s.ThreadFor(ctx, actor, id)
		if err != nil {
			if errors.Is(err, ErrNoSuchThread) {
				continue
			}
			return nil, err
		}
		view := v
		out = append(out, Frame{Seq: v.LastSeq, Stream: StreamThread, Thread: id, Thr: &view})
	}
	return out, nil
}

// Search runs a full-text query over the messages in the caller's own threads.
//
// It is the replacement for the ambient channel history a bot used to be handed
// unasked. Scoping it to the caller's threads is not a nicety: participation is
// the only authorization boundary left, and a search that crossed it would be
// the one hole in it.
func (s *Store) Search(ctx context.Context, actor, query string, limit int) ([]Message, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("chat: search needs a query")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT m.seq, m.thread_id, m.author_id, m.body, m.kind, m.created_at
		   FROM messages_fts f
		   JOIN messages m ON m.seq = f.rowid
		   JOIN participants p ON p.thread_id = m.thread_id AND p.actor_id = ?
		  WHERE messages_fts MATCH ?
		  ORDER BY m.seq DESC
		  LIMIT ?`, actor, ftsQuery(query), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []Message{}
	for rows.Next() {
		var m Message
		var created int64
		if err := rows.Scan(&m.Seq, &m.Thread, &m.Author, &m.Body, &m.Kind, &created); err != nil {
			return nil, err
		}
		m.Created = time.UnixMilli(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ftsQuery makes a user's words safe for FTS5's query grammar by quoting each
// one. A model writing `auth.Refresh(` or `NOT` would otherwise get a syntax
// error, or a query meaning something it did not intend.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " ")
}

// Actors lists the identities the store knows.
func (s *Store) Actors(ctx context.Context) ([]Actor, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT id, kind, name, role, present, created_at FROM actors ORDER BY kind, name`)
	if err != nil {
		return nil, err
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

// ThreadsFor lists the thread ids an actor participates in. It is what a bot's
// tools list before naming one.
func (s *Store) ThreadsFor(ctx context.Context, actor string) ([]string, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT t.id FROM threads t
		   JOIN participants p ON p.thread_id = t.id AND p.actor_id = ?
		  WHERE t.archived_at IS NULL
		  ORDER BY t.last_seq DESC`, actor)
	if err != nil {
		return nil, err
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
