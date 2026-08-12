package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// BlobThreshold bounds how much of a tool result is stored inline. Anything
// larger goes to the blobs table and is fetched when someone expands the call.
//
// It is exported because the split is made by whoever builds the record - only
// they know which event kinds carry a tool result and which field of one may be
// trimmed - and both sides must agree on the number for the timeline to stay
// small enough to ship on a reconnect. It matches uisession's own threshold for
// the session journal, which does the same job for the interactive workspace.
const BlobThreshold = 2048

// pruneBatch bounds one delete transaction. Pruning a fleet's accumulated
// timeline in a single statement held the writer - and therefore every bot's
// next message - for seconds.
const pruneBatch = 5000

// EventRecord is one step of a bot's turn, ready to store. Payload is the event
// as the browser already decodes it, so there is no second wire format to keep
// in step with the front-end; this package never looks inside it beyond
// checking that it is JSON at all.
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
	Payload json.RawMessage
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
	err := s.write(ctx, "begin turn", func(tx *sql.Tx, frames *[]Frame) error {
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

// AddUsage accumulates what one model call cost onto the turn.
//
// A zero Usage still counts the call, in uncounted, so a total can never
// quietly understate the spend by dropping the calls the provider reported
// nothing for. A turn seq that is not the caller's own is refused: they are
// small consecutive integers, so without the check any actor could bill or
// corrupt any other's accounting by guessing.
func (s *Store) AddUsage(ctx context.Context, actor string, turnSeq uint64, u Usage, model string) error {
	return s.write(ctx, "add usage", func(tx *sql.Tx, _ *[]Frame) error {
		if _, err := requireOwnTurn(ctx, tx, turnSeq, actor); err != nil {
			return err
		}
		uncounted := 0
		if u.InputTokens == 0 && u.OutputTokens == 0 {
			uncounted = 1
		}
		model = SanitizeField(model)
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

// EndTurn closes a turn.
//
// The first close wins: CloseStaleTurns may already have ended it, and a late
// success from the goroutine that was interrupted must not overwrite the reason
// it stopped. A turn that is never ended at all - because the process died - is
// closed at the next startup.
func (s *Store) EndTurn(ctx context.Context, actor string, turnSeq uint64, turnErr string) error {
	return s.write(ctx, "end turn", func(tx *sql.Tx, frames *[]Frame) error {
		threadID, err := requireOwnTurn(ctx, tx, turnSeq, actor)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE turns SET ended_at = ?, error = ? WHERE turn_seq = ? AND ended_at IS NULL`,
			s.now().UnixMilli(), SanitizeField(turnErr), turnSeq); err != nil {
			return err
		}
		return s.publishThread(ctx, tx, frames, threadID)
	})
}

// requireOwnTurn resolves a turn the actor owns, and returns its thread.
func requireOwnTurn(ctx context.Context, tx *sql.Tx, turnSeq uint64, actor string) (string, error) {
	var threadID, owner string
	err := tx.QueryRowContext(ctx,
		`SELECT thread_id, actor_id FROM turns WHERE turn_seq = ?`, turnSeq).Scan(&threadID, &owner)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != actor) {
		return "", ErrNoSuchTurn
	}
	if err != nil {
		return "", err
	}
	return threadID, nil
}

// CloseStaleTurns ends every turn still open, and reports how many there were.
//
// The fleet calls it at startup: a turn with no process behind it is not
// running, and an inbox that says otherwise is worse than one that says
// nothing. It publishes each affected thread, because "working" is derived from
// this table and an attached browser would otherwise keep the run dot spinning
// until some unrelated write touched the row.
func (s *Store) CloseStaleTurns(ctx context.Context) (int, error) {
	var n int
	err := s.write(ctx, "close stale turns", func(tx *sql.Tx, frames *[]Frame) error {
		threads, err := openTurnThreads(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE turns SET ended_at = ?, error = ? WHERE ended_at IS NULL`,
			s.now().UnixMilli(), "interrupted by a restart")
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}
		n = int(rows)
		for _, id := range threads {
			if err := s.publishThread(ctx, tx, frames, id); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}

func openTurnThreads(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT thread_id FROM turns WHERE ended_at IS NULL ORDER BY thread_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AppendEvent records one step of a turn and returns its sequence number.
func (s *Store) AppendEvent(ctx context.Context, rec EventRecord) (uint64, error) {
	switch {
	case rec.Thread == "" || rec.Kind == "":
		return 0, invalid("an event needs a thread and a kind")
	case len(rec.Payload) > MaxEventBytes:
		return 0, invalid("an event payload may be at most %s", HumanSize(MaxEventBytes))
	case len(rec.Blob) > MaxEventBlobBytes:
		return 0, invalid("an event blob may be at most %s", HumanSize(MaxEventBlobBytes))
	case !json.Valid(rec.Payload):
		// One unparseable row would make every later Timeline response for this
		// thread un-encodable, permanently, because the payload is re-emitted as
		// raw JSON rather than re-encoded.
		return 0, invalid("an event payload must be JSON")
	}
	var seq uint64
	err := s.write(ctx, "append event", func(tx *sql.Tx, frames *[]Frame) error {
		// The same check BeginTurn makes. Without it a non-participant could
		// write rendered content into someone else's timeline, attribute it to
		// any bot, and use the foreign key to learn which thread ids are real.
		if err := requireParticipant(ctx, tx, rec.Thread, rec.Actor); err != nil {
			return err
		}
		if rec.TurnSeq != 0 {
			threadID, err := requireOwnTurn(ctx, tx, rec.TurnSeq, rec.Actor)
			if err != nil {
				return err
			}
			if threadID != rec.Thread {
				return invalid("turn %d belongs to another thread", rec.TurnSeq)
			}
		}
		var err error
		if seq, err = nextSeq(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO events (seq, thread_id, actor_id, turn_seq, kind, payload, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			seq, rec.Thread, rec.Actor, rec.TurnSeq, rec.Kind, []byte(rec.Payload),
			s.now().UnixMilli()); err != nil {
			return err
		}
		if len(rec.Blob) > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO blobs (seq, body) VALUES (?, ?)`,
				seq, rec.Blob); err != nil {
				return err
			}
		}
		// An event does not publish a thread frame - a turn produces hundreds of
		// them and the row does not change - so it addresses itself.
		audience, err := participantsOf(ctx, tx, rec.Thread)
		if err != nil {
			return err
		}
		*frames = append(*frames, Frame{
			Seq: seq, Stream: StreamEvent, ThreadID: rec.Thread, Event: rec.Payload, To: audience,
		})
		return nil
	})
	return seq, err
}

// Blob returns the untruncated body of an oversized tool result. It answers
// ErrNoSuchThread for a missing blob, a missing event and a caller who is not a
// participant alike, for the reason on that sentinel.
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
	if err != nil {
		return nil, fmt.Errorf("chat: read blob: %w", err)
	}
	return body, nil
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
		return nil, fmt.Errorf("chat: read turns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Turn{}
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
// and it is small. How an agent arrived at it is large - five bots on
// heartbeats write a timeline all day - and it stops being worth its disk
// within days, which is exactly the tradeoff a fixed-size ring would get wrong
// in both directions.
//
// It runs in batches. A fleet's accumulated timeline is millions of rows, and
// deleting them in one transaction holds the single writer - and therefore
// every bot's next message - for as long as it takes. Events belonging to a
// turn that is still running are left alone, so a live run is never shown with
// an amputated history.
//
// It reports what it removed so the caller can log it: a store that silently
// discards history reads as one that never had it.
func (s *Store) Prune(ctx context.Context, before time.Time) (events, blobs int, err error) {
	cutoff := before.UnixMilli()
	for {
		var batchEvents, batchBlobs int
		err := s.write(ctx, "prune", func(tx *sql.Tx, _ *[]Frame) error {
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM blobs WHERE seq IN (`+prunable+`)`,
				cutoff, pruneBatch).Scan(&batchBlobs); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx,
				`DELETE FROM events WHERE seq IN (`+prunable+`)`, cutoff, pruneBatch)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			batchEvents = int(n)
			return err
		})
		if err != nil {
			return events, blobs, err
		}
		events += batchEvents
		blobs += batchBlobs
		if batchEvents < pruneBatch {
			break
		}
	}
	if events > 0 {
		// Deleting rows returns their pages to SQLite's free list, not to the
		// filesystem. The schema asks for incremental auto-vacuum precisely so
		// this is one cheap statement rather than a whole-file rewrite.
		if _, err := s.w.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return events, blobs, fmt.Errorf("chat: reclaim pruned space: %w", err)
		}
	}
	return events, blobs, nil
}

// prunable is one batch of events old enough to drop and not belonging to a
// turn that is still running.
const prunable = `SELECT e.seq FROM events e
	 LEFT JOIN turns t ON t.turn_seq = e.turn_seq
	 WHERE e.created_at < ? AND (t.turn_seq IS NULL OR t.ended_at IS NOT NULL)
	 LIMIT ?`
