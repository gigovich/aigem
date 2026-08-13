package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

// artifactBatch is the same bound for stored diffs, and it is far smaller
// because the rows are. An event is capped at 64KiB with anything larger held
// in blobs; an artifact carries both sides of a file inline, so a batch of the
// events' size would be gigabytes of deletes in one transaction on the writer
// the whole fleet is queued behind.
const artifactBatch = 200

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

	// Step says this event counts towards the turn's step total, and Tool says
	// it is a tool call. Both are the producer's judgement rather than a switch
	// on Kind here: which kinds are steps belongs to the event vocabulary, and
	// this package deliberately knows nothing about it.
	Step bool
	Tool bool
	// Plan is the working plan this event set, when it set one. Stored on the
	// turn so the panel can show it without replaying the timeline.
	Plan json.RawMessage
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

// AddUsage adds what one or more of a turn's model calls cost onto it.
//
// u carries its own Calls and Uncounted rather than this counting one call per
// AddUsage, because only whoever made the calls knows how many there were: the
// bot batches, to keep the fleet's single writer off the goroutine holding a
// provider response open, and arrives with a dozen calls folded into one set of
// numbers.
//
// A turn seq that is not the caller's own is refused: they are small
// consecutive integers, so without the check any actor could bill or corrupt
// any other's accounting by guessing. A turn that has ended takes no more
// spend, on the same reasoning EndTurn closes only once - a number that keeps
// moving after the work stopped is not a record of it.
func (s *Store) AddUsage(ctx context.Context, actor string, turnSeq uint64, u Usage, model string) error {
	return s.write(ctx, "add usage", func(tx *sql.Tx, _ *[]Frame) error {
		if _, err := requireOwnTurn(ctx, tx, turnSeq, actor); err != nil {
			return err
		}
		model = SanitizeField(model)
		_, err := tx.ExecContext(ctx,
			`UPDATE turns SET
			   in_tokens = in_tokens + ?, cached_tokens = cached_tokens + ?,
			   out_tokens = out_tokens + ?, calls = calls + ?,
			   uncounted = uncounted + ?,
			   model = CASE WHEN ? = '' THEN model ELSE ? END
			 WHERE turn_seq = ? AND ended_at IS NULL`,
			u.InputTokens, u.CachedTokens, u.OutputTokens, u.Calls, u.Uncounted,
			model, model, turnSeq)
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

// requireTurnInThread checks that a turn an actor is filing something under is
// their own and belongs to this thread. A zero turn means "outside any run" and
// is always allowed.
func requireTurnInThread(ctx context.Context, tx *sql.Tx, turnSeq uint64, actor, threadID string) error {
	if turnSeq == 0 {
		return nil
	}
	owning, err := requireOwnTurn(ctx, tx, turnSeq, actor)
	if err != nil {
		return err
	}
	if owning != threadID {
		return invalid("turn %d belongs to another thread", turnSeq)
	}
	return nil
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
	case len(rec.Plan) > MaxEventBytes:
		return 0, invalid("a plan may be at most %s", HumanSize(MaxEventBytes))
	case len(rec.Plan) > 0 && !json.Valid(rec.Plan):
		// Stored raw and re-emitted raw, exactly as the payload is, so the same
		// reasoning applies: one bad row would break the turns response forever.
		return 0, invalid("a plan must be JSON")
	}
	var seq uint64
	err := s.write(ctx, "append event", func(tx *sql.Tx, frames *[]Frame) error {
		// The same check BeginTurn makes. Without it a non-participant could
		// write rendered content into someone else's timeline, attribute it to
		// any bot, and use the foreign key to learn which thread ids are real.
		if err := requireParticipant(ctx, tx, rec.Thread, rec.Actor); err != nil {
			return err
		}
		if err := requireTurnInThread(ctx, tx, rec.TurnSeq, rec.Actor, rec.Thread); err != nil {
			return err
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
		// The turn's summary line, kept current in the transaction that already
		// has the row - the same bargain last_seq and last_text struck for the
		// inbox. Counters only move while the turn is open, so a late event from
		// a goroutine that outlived its run cannot inflate a closed turn's total.
		if rec.TurnSeq != 0 && (rec.Step || rec.Tool || len(rec.Plan) > 0) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE turns SET
				   steps = steps + ?, tool_calls = tool_calls + ?,
				   plan = CASE WHEN ? = '' THEN plan ELSE ? END
				 WHERE turn_seq = ? AND ended_at IS NULL`,
				boolInt(rec.Step), boolInt(rec.Tool),
				string(rec.Plan), string(rec.Plan), rec.TurnSeq); err != nil {
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
			Seq: seq, Stream: StreamEvent, ThreadID: rec.Thread, Turn: rec.TurnSeq,
			Event: rec.Payload, To: audience,
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

// PutArtifact records a file a turn changed, and counts it on the turn.
//
// A path already recorded for this turn keeps the content it had when the turn
// first touched it, so a turn that edited one file five times still shows one
// diff of its whole effect - the same rule uisession applies to a session's
// artifacts, for the same reason.
//
// It reports whether the file was recorded. A turn past its file cap, or one
// that has already ended, is not an error - there is nothing the caller could
// do about either - but the caller must not then write a timeline step claiming
// a diff the panel has no row for.
//
// a.Turn and a.Changed are the store's to set and are ignored.
func (s *Store) PutArtifact(ctx context.Context, actor, threadID string, turnSeq uint64,
	a Artifact) (stored bool, err error) {
	// Cleaned, not shortened - see ReadablePath. The caller is expected to have
	// done this already so its timeline event carries the same string; doing it
	// again here is what makes the store's own guarantee true either way.
	a.Path = ReadablePath(a.Path)
	switch {
	case turnSeq == 0:
		return false, invalid("an artifact needs the turn that changed it")
	case a.Path == "":
		return false, invalid("an artifact needs a path")
	}
	// Kept as a path with no content rather than refused: which files a turn
	// touched is the half of the fact worth keeping, and a diff this large was
	// never going to be drawn in a browser anyway.
	tooBig := len(a.Old) > MaxArtifactBytes || len(a.New) > MaxArtifactBytes
	if tooBig {
		a.Old, a.New = "", ""
	}
	err = s.write(ctx, "put artifact", func(tx *sql.Tx, _ *[]Frame) error {
		if err := requireParticipant(ctx, tx, threadID, actor); err != nil {
			return err
		}
		if err := requireTurnInThread(ctx, tx, turnSeq, actor, threadID); err != nil {
			return err
		}
		// Only while the turn is open, on the same reasoning AddUsage and
		// AppendEvent carry: a row that keeps growing after the work stopped is
		// not a record of it - and because pruning ages artifacts by changed_at,
		// a late write would also push the row's expiry out another month.
		var open bool
		if err := tx.QueryRowContext(ctx,
			`SELECT ended_at IS NULL FROM turns WHERE turn_seq = ?`, turnSeq).Scan(&open); err != nil {
			return err
		}
		if !open {
			return nil
		}
		stored = true
		var known bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM artifacts WHERE turn_seq = ? AND path = ?)`,
			turnSeq, a.Path).Scan(&known); err != nil {
			return err
		}
		if !known {
			// The turn's own counter, not count(*) over the artifacts. That table
			// is WITHOUT ROWID, so its primary key is the table and counting a
			// turn's rows walks records carrying whole file bodies - a third of a
			// second per call at the size cap, inside the transaction the entire
			// fleet queues behind, on every file a turn writes.
			var have int
			if err := tx.QueryRowContext(ctx,
				`SELECT files FROM turns WHERE turn_seq = ?`, turnSeq).Scan(&have); err != nil {
				return err
			}
			// Dropped rather than refused: a bot cannot act on being told its two
			// hundred and first file was not recorded, and failing the write would
			// fail the edit that caused it. The count stops here too, so the
			// figure on the summary line is the number of files the panel can
			// actually list rather than the number of times one was written.
			if have >= MaxTurnArtifacts {
				stored = false
				return nil
			}
		}
		// Only new and changed_at move on a later edit. old is what the file held
		// before the turn first touched it, and truncated is sticky: once a side
		// has been dropped there is nothing left to diff the next edit against.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO artifacts
			   (turn_seq, thread_id, path, created, truncated, changed_at, old, new)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (turn_seq, path) DO UPDATE SET
			   truncated  = truncated | excluded.truncated,
			   new        = CASE WHEN truncated | excluded.truncated = 1 THEN ''
			                     ELSE excluded.new END,
			   old        = CASE WHEN truncated | excluded.truncated = 1 THEN ''
			                     ELSE old END,
			   changed_at = excluded.changed_at`,
			turnSeq, threadID, a.Path, boolInt(a.Created), boolInt(tooBig),
			s.now().UnixMilli(), a.Old, a.New); err != nil {
			return err
		}
		if known {
			return nil
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE turns SET files = files + 1 WHERE turn_seq = ?`, turnSeq)
		return err
	})
	if err != nil {
		return false, err
	}
	return stored, nil
}

// Artifacts returns the files a turn changed. A zero turn means the newest turn
// in the thread that changed anything, which is what the panel asks for.
//
// Content comes back only for a path asked for by name, exactly as the session
// daemon's own artifact route behaves: the list is opened far more often than
// any one diff is read.
func (s *Store) Artifacts(ctx context.Context, actor, threadID string, turnSeq uint64,
	path string) ([]Artifact, error) {
	if err := s.requireParticipantR(ctx, threadID, actor); err != nil {
		return nil, err
	}
	if turnSeq == 0 {
		if err := s.r.QueryRowContext(ctx,
			`SELECT coalesce(max(turn_seq), 0) FROM artifacts WHERE thread_id = ?`,
			threadID).Scan(&turnSeq); err != nil {
			return nil, fmt.Errorf("chat: read artifacts: %w", err)
		}
		if turnSeq == 0 {
			return []Artifact{}, nil
		}
	}
	body := "'', ''"
	args := []any{threadID, turnSeq}
	where := ""
	if path != "" {
		body = "old, new"
		where = " AND path = ?"
		args = append(args, path)
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT turn_seq, path, created, truncated, changed_at, `+body+`
		   FROM artifacts WHERE thread_id = ? AND turn_seq = ?`+where+`
		  ORDER BY path`, args...)
	if err != nil {
		return nil, fmt.Errorf("chat: read artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		var changed int64
		if err := rows.Scan(&a.Turn, &a.Path, &a.Created, &a.Truncated, &changed,
			&a.Old, &a.New); err != nil {
			return nil, err
		}
		a.Changed = time.UnixMilli(changed)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Turns returns a page of a thread's turns, newest first, ending just below
// before. A zero before starts at the newest.
//
// Paged like the messages it accompanies. Nothing prunes this table and every
// inbound message is a run, so a thread that has been worked for a month has
// thousands of rows, and the panel wants the handful covering what is on screen.
func (s *Store) Turns(ctx context.Context, actor, threadID string, before uint64, limit int) (
	turns []Turn, cursor uint64, more bool, err error) {
	if err := s.requireParticipantR(ctx, threadID, actor); err != nil {
		return nil, 0, false, err
	}
	limit = clampLimit(limit, 100, 500)
	rows, err := s.r.QueryContext(ctx,
		`SELECT turn_seq, thread_id, actor_id, started_at, ended_at,
		        in_tokens, cached_tokens, out_tokens, calls, uncounted, model, error,
		        steps, tool_calls, files, plan
		   FROM turns
		  WHERE thread_id = ? AND turn_seq < ?
		  ORDER BY turn_seq DESC
		  LIMIT ?`, threadID, sqlSeq(before), limit+1)
	if err != nil {
		return nil, 0, false, fmt.Errorf("chat: read turns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Turn{}
	for rows.Next() {
		var t Turn
		var started int64
		var ended sql.NullInt64
		var plan string
		if err := rows.Scan(&t.Seq, &t.Thread, &t.Actor, &started, &ended,
			&t.Usage.InputTokens, &t.Usage.CachedTokens, &t.Usage.OutputTokens,
			&t.Usage.Calls, &t.Usage.Uncounted, &t.Model, &t.Error,
			&t.Steps, &t.Tools, &t.Files, &plan); err != nil {
			return nil, 0, false, err
		}
		t.Started = time.UnixMilli(started)
		if ended.Valid {
			t.Ended = time.UnixMilli(ended.Int64)
		}
		if plan != "" {
			t.Plan = json.RawMessage(plan)
		}
		out = append(out, t)
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

// Spend is what the work in a thread has cost, summed over its turns. Turns
// counts only the ones that spent something: a run that failed before it
// reached the provider is not work the account paid for.
type Spend struct {
	Usage Usage `json:"usage,omitzero"`
	// Turns counts the runs that reached the provider. Runs counts every run in
	// the thread. They are both here because they answer different questions and
	// were being confused for each other: a turn killed before its first flush
	// spent nothing, so a thread of 100 runs routinely reports 94 turns - which
	// is the right number to put beside a cost and the wrong one to label
	// "turns" over a list of who ran what.
	Turns  int      `json:"turns"`
	Runs   int      `json:"runs"`
	Models []string `json:"models,omitempty"`
}

// Spend sums a thread's turns.
//
// It is a query rather than a sum over Turns because a caller that wants one
// line wants one line. Nothing prunes the turns table - a thread is long-lived
// and every inbound message is a run - so summing on the client would make
// reading a thread cost its entire history of runs.
func (s *Store) Spend(ctx context.Context, actor, threadID string) (Spend, error) {
	if err := s.requireParticipantR(ctx, threadID, actor); err != nil {
		return Spend{}, err
	}
	var sp Spend
	if err := s.r.QueryRowContext(ctx,
		`SELECT coalesce(sum(in_tokens), 0), coalesce(sum(cached_tokens), 0),
		        coalesce(sum(out_tokens), 0), coalesce(sum(calls), 0),
		        coalesce(sum(uncounted), 0),
		        count(*) FILTER (WHERE `+turnSpent+`), count(*)
		   FROM turns WHERE thread_id = ?`, threadID).
		Scan(&sp.Usage.InputTokens, &sp.Usage.CachedTokens, &sp.Usage.OutputTokens,
			&sp.Usage.Calls, &sp.Usage.Uncounted, &sp.Turns, &sp.Runs); err != nil {
		return Spend{}, fmt.Errorf("chat: read spend: %w", err)
	}
	// Every model the thread ran on, not the last one, which would put all of
	// its tokens under whichever turn happened to finish last. Ordered, so two
	// identical reads cannot disagree.
	rows, err := s.r.QueryContext(ctx,
		`SELECT DISTINCT model FROM turns
		  WHERE thread_id = ? AND model <> '' AND `+turnSpent+` ORDER BY model`, threadID)
	if err != nil {
		return Spend{}, fmt.Errorf("chat: read spend models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return Spend{}, err
		}
		sp.Models = append(sp.Models, model)
	}
	return sp, rows.Err()
}

// turnSpent is a turn that reached the provider. It is one string so the sum
// and the model list cannot come to disagree about which turns they describe.
const turnSpent = `NOT (in_tokens = 0 AND cached_tokens = 0 AND out_tokens = 0
	AND calls = 0 AND uncounted = 0)`

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
func (s *Store) Prune(ctx context.Context, before time.Time) (events, blobs, artifacts int, err error) {
	cutoff := before.UnixMilli()
	// Artifacts age out on the same terms and for the same reason: they are the
	// diff behind a step in a timeline that is about to stop existing, and the
	// turns they hang off are never pruned, so nothing else would ever reclaim
	// them. Files belonging to a running turn are left alone.
	artifacts, err = s.pruneArtifacts(ctx, cutoff)
	if err != nil {
		return 0, 0, 0, err
	}
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
			return events, blobs, artifacts, err
		}
		events += batchEvents
		blobs += batchBlobs
		if batchEvents < pruneBatch {
			break
		}
	}
	if events > 0 || artifacts > 0 {
		if err := s.reclaim(ctx); err != nil {
			return events, blobs, artifacts, err
		}
	}
	return events, blobs, artifacts, nil
}

// vacuumPages is how much of the free list one reclaim statement returns to the
// filesystem. At SQLite's 4KiB page that is 8MiB a step, so the writer the whole
// fleet queues behind is released between steps as it is between the deletes.
const vacuumPages = 2000

// reclaim returns the pages a prune freed to the filesystem, in bounded steps.
//
// It does nothing at all on a database that is not in incremental auto-vacuum
// mode, and that is the common case rather than the exotic one: the pragma in
// 0001_init.sql is a no-op, because the driver applies the DSN's journal_mode
// pragma first and that initialises the file - after which auto_vacuum can only
// be changed by a full VACUUM. New stores get it from the DSN; every store made
// before that does not, and there `incremental_vacuum` frees nothing.
//
// Which is why the loop stops on the free list failing to shrink rather than on
// it reaching zero. Trusting the statement to make progress turned a harmless
// no-op into a hot loop on the single writer, forever, on every prune.
func (s *Store) reclaim(ctx context.Context) error {
	prev := -1
	for {
		var left int
		if err := s.w.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&left); err != nil {
			return fmt.Errorf("chat: reclaim pruned space: %w", err)
		}
		if left == 0 || (prev >= 0 && left >= prev) {
			return nil
		}
		prev = left
		if _, err := s.w.ExecContext(ctx, `PRAGMA incremental_vacuum(`+
			strconv.Itoa(vacuumPages)+`)`); err != nil {
			return fmt.Errorf("chat: reclaim pruned space: %w", err)
		}
	}
}

// pruneArtifacts drops stored diffs older than the cutoff, in the same batches
// and on the same terms as the events.
func (s *Store) pruneArtifacts(ctx context.Context, cutoff int64) (int, error) {
	total := 0
	for {
		var batch int
		err := s.write(ctx, "prune artifacts", func(tx *sql.Tx, _ *[]Frame) error {
			// By primary key, not rowid: the table is WITHOUT ROWID and has none.
			res, err := tx.ExecContext(ctx,
				`DELETE FROM artifacts WHERE (turn_seq, path) IN (
				   SELECT a.turn_seq, a.path FROM artifacts a
				    LEFT JOIN turns t ON t.turn_seq = a.turn_seq
				    WHERE a.changed_at < ? AND (t.turn_seq IS NULL OR t.ended_at IS NOT NULL)
				    LIMIT ?)`, cutoff, artifactBatch)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			batch = int(n)
			return err
		})
		if err != nil {
			return total, err
		}
		total += batch
		if batch < artifactBatch {
			return total, nil
		}
	}
}

// prunable is one batch of events old enough to drop and not belonging to a
// turn that is still running.
const prunable = `SELECT e.seq FROM events e
	 LEFT JOIN turns t ON t.turn_seq = e.turn_seq
	 WHERE e.created_at < ? AND (t.turn_seq IS NULL OR t.ended_at IS NOT NULL)
	 LIMIT ?`
