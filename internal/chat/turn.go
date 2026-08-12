package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BlobThreshold bounds how much of a tool result is stored inline. Anything
// larger goes to the blobs table and is fetched when someone expands the call.
//
// It is exported because the split is made by whoever builds the record - only
// they know which event kinds carry a tool result - and both sides must agree
// on the number for the timeline to stay small enough to ship on a reconnect.
const BlobThreshold = 2048

// EventRecord is one step of a bot's turn, ready to store. Payload is the event
// as the browser already decodes it, so there is no second wire format to keep
// in step with the front-end; this package never looks inside it.
//
// Blob is the untruncated body of an oversized tool result, or nil. The caller
// truncates Payload and hands the whole thing here, because deciding what may
// be trimmed needs to know what the event means, and that knowledge belongs
// with the producer rather than with the store.
type EventRecord struct {
	Thread  string
	Actor   string
	TurnSeq uint64
	Kind    string
	Payload []byte
	Blob    []byte
}

// BeginTurn opens a turn and returns its sequence number, which is also the id
// every event of that turn is filed under.
//
// The row exists for the whole run, so "amiran is working" is a fact anyone can
// read rather than a ping that keeps having to be re-sent - and one that would
// still be claiming to be true some seconds after the process died.
func (s *Store) BeginTurn(ctx context.Context, threadID, actor string) (uint64, error) {
	var turnSeq uint64
	err := s.write(ctx, func(tx *sql.Tx, frames *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, actor); err != nil {
			return err
		}
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return err
		}
		turnSeq = seq
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO turns (turn_seq, thread_id, actor_id, started_at) VALUES (?, ?, ?, ?)`,
			seq, threadID, actor, s.now().UnixMilli()); err != nil {
			return err
		}
		return s.publishThread(ctx, tx, frames, threadID)
	})
	return turnSeq, err
}

// AddUsage accumulates what one model call cost onto the turn. A zero Usage
// still counts the call, in uncounted, so a total can never quietly understate
// the spend by dropping the calls the provider reported nothing for.
func (s *Store) AddUsage(ctx context.Context, turnSeq uint64, u Usage, model string) error {
	return s.write(ctx, func(tx *sql.Tx, _ *[]Frame) error {
		uncounted := 0
		if u.InputTokens == 0 && u.OutputTokens == 0 {
			uncounted = 1
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE turns SET
			   in_tokens = in_tokens + ?, cached_tokens = cached_tokens + ?,
			   out_tokens = out_tokens + ?, calls = calls + 1,
			   uncounted = uncounted + ?,
			   model = CASE WHEN ? = '' THEN model ELSE ? END
			 WHERE turn_seq = ?`,
			u.InputTokens, u.CachedTokens, u.OutputTokens, uncounted, model, model, turnSeq)
		return err
	})
}

// EndTurn closes a turn. A turn that is never ended - because the process died
// mid-run - is closed by ReopenTurns at the next startup rather than left
// claiming to be live forever.
func (s *Store) EndTurn(ctx context.Context, turnSeq uint64, turnErr string) error {
	return s.write(ctx, func(tx *sql.Tx, frames *[]Frame) error {
		var threadID string
		err := tx.QueryRowContext(ctx, `SELECT thread_id FROM turns WHERE turn_seq = ?`,
			turnSeq).Scan(&threadID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE turns SET ended_at = ?, error = ? WHERE turn_seq = ? AND ended_at IS NULL`,
			s.now().UnixMilli(), turnErr, turnSeq); err != nil {
			return err
		}
		return s.publishThread(ctx, tx, frames, threadID)
	})
}

// CloseStaleTurns ends every turn still open, and reports how many there were.
// The fleet calls it at startup: a turn with no process behind it is not
// running, and an inbox that says otherwise is worse than one that says nothing.
func (s *Store) CloseStaleTurns(ctx context.Context) (int, error) {
	var n int
	err := s.write(ctx, func(tx *sql.Tx, _ *[]Frame) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE turns SET ended_at = ?, error = ?
			 WHERE ended_at IS NULL`,
			s.now().UnixMilli(), "interrupted by a restart")
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		n = int(rows)
		return err
	})
	return n, err
}

// AppendEvent records one step of a turn and returns its sequence number.
func (s *Store) AppendEvent(ctx context.Context, rec EventRecord) (uint64, error) {
	if rec.Thread == "" || rec.Kind == "" {
		return 0, errors.New("chat: an event needs a thread and a kind")
	}
	var seq uint64
	err := s.write(ctx, func(tx *sql.Tx, frames *[]Frame) error {
		var err error
		if seq, err = nextSeq(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (seq, thread_id, actor_id, turn_seq, kind, payload, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			seq, rec.Thread, rec.Actor, rec.TurnSeq, rec.Kind, rec.Payload,
			s.now().UnixMilli()); err != nil {
			return err
		}
		if len(rec.Blob) > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO blobs (seq, body) VALUES (?, ?)`,
				seq, rec.Blob); err != nil {
				return err
			}
		}
		*frames = append(*frames, Frame{
			Seq: seq, Stream: StreamEvent, Thread: rec.Thread, Event: rec.Payload,
		})
		return nil
	})
	return seq, err
}

// Blob returns the untruncated body of an oversized tool result.
func (s *Store) Blob(ctx context.Context, actor, threadID string, seq uint64) ([]byte, error) {
	var body []byte
	err := s.r.QueryRowContext(ctx,
		`SELECT b.body FROM blobs b
		   JOIN events e ON e.seq = b.seq
		   JOIN participants p ON p.thread_id = e.thread_id AND p.actor_id = ?
		  WHERE b.seq = ? AND e.thread_id = ?`,
		actor, seq, threadID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchThread
	}
	return body, err
}

// Turns returns a thread's turns, oldest first, with what each one spent.
func (s *Store) Turns(ctx context.Context, actor, threadID string) ([]Turn, error) {
	if err := s.requireParticipantR(ctx, threadID, actor); err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT turn_seq, thread_id, actor_id, started_at, ended_at,
		        in_tokens, cached_tokens, out_tokens, calls, uncounted, model, error
		   FROM turns WHERE thread_id = ? ORDER BY turn_seq`, threadID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Turn
	for rows.Next() {
		var t Turn
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&t.Seq, &t.Thread, &t.Actor, &started, &ended,
			&t.Usage.InputTokens, &t.Usage.CachedTokens, &t.Usage.OutputTokens,
			&t.Usage.Calls, &t.Usage.Uncounted, &t.Model, &t.Error); err != nil {
			return nil, err
		}
		t.Started = time.UnixMilli(started)
		if ended.Valid {
			t.Ended = time.UnixMilli(ended.Int64)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Prune deletes timeline events older than before, and the blobs hanging off
// them. Messages are never pruned.
//
// The two are kept on different terms on purpose. What was said is the record,
// and it is small. How an agent arrived at it is large - five bots on heartbeats
// write a timeline all day - and it stops being worth its disk within days,
// which is exactly the tradeoff a fixed-size ring would get wrong in both
// directions. Pruning reports what it removed so the caller can log it: a store
// that silently discards history reads as one that never had it.
func (s *Store) Prune(ctx context.Context, before time.Time) (events, blobs int, err error) {
	err = s.write(ctx, func(tx *sql.Tx, _ *[]Frame) error {
		cutoff := before.UnixMilli()
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM blobs b JOIN events e ON e.seq = b.seq
			  WHERE e.created_at < ?`, cutoff).Scan(&blobs); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE created_at < ?`, cutoff)
		if err != nil {
			return fmt.Errorf("chat: prune events: %w", err)
		}
		n, err := res.RowsAffected()
		events = int(n)
		return err
	})
	return events, blobs, err
}
