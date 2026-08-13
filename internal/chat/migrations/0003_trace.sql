-- What a turn did, made cheap enough to draw.
--
-- The timeline already holds every step, but the collapsed summary line
-- ("14 steps - 6 tools - 2 files") is drawn for every bot message on screen,
-- and computing it from the events would mean pulling a long thread's whole
-- history to render a hundred one-line summaries. These are denormalised in the
-- transaction AppendEvent already opens, which is the same bargain last_seq and
-- last_text struck for the inbox.

-- Which turn produced a message. Messages and events interleave in one global
-- sequence, so a reader could bracket a turn by its start and end - but a bot
-- may post several times inside one turn, and a turn that crashed never wrote
-- its end. The link is recorded rather than inferred.
--
-- 0 means "said outside a turn": every message the operator writes, and every
-- message from before this migration.
ALTER TABLE messages ADD COLUMN turn_seq INTEGER NOT NULL DEFAULT 0;

ALTER TABLE turns ADD COLUMN steps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN tool_calls INTEGER NOT NULL DEFAULT 0;
ALTER TABLE turns ADD COLUMN files INTEGER NOT NULL DEFAULT 0;
-- The working plan as it stood when this turn last wrote one, as the JSON the
-- browser already decodes. A turn that never called the plan tool stores
-- nothing, and the panel shows the newest turn that did - carrying it forward
-- at write time would copy a stale plan onto every heartbeat.
ALTER TABLE turns ADD COLUMN plan TEXT NOT NULL DEFAULT '';

-- Files a turn changed, with the content on both sides.
--
-- Per turn, not per thread. A session's artifacts answer "what did this run
-- do", and a run is minutes long; a bot thread lives for weeks, and the whole
-- of its effect on a tree is neither a question anyone asks nor a quantity
-- anything bounds. Keyed to the turn, it is the same fact as the "2 files" on
-- that turn's summary line, and it ages out with the timeline it belongs to.
--
-- old is what the file held before this turn first touched it, so a turn that
-- edited one file five times still shows one diff of its whole effect.
-- truncated marks a file whose content was too large to keep. It is sticky:
-- once a side has been dropped there is nothing to diff the next edit against,
-- and showing the second edit alone would render the whole file as created.
-- Every column the list query reads comes before old and new. SQLite stores a
-- record's columns in order and spills the tail into overflow pages, so a
-- changed_at declared after two blobs is only reachable by walking the chain
-- the list is deliberately not fetching.
CREATE TABLE artifacts (
  turn_seq   INTEGER NOT NULL,
  thread_id  TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  path       TEXT    NOT NULL,
  created    INTEGER NOT NULL DEFAULT 0,
  truncated  INTEGER NOT NULL DEFAULT 0,
  changed_at INTEGER NOT NULL,
  old        TEXT    NOT NULL DEFAULT '',
  new        TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (turn_seq, path)
) WITHOUT ROWID;
-- The panel asks for one turn's files, newest turn first.
CREATE INDEX artifacts_by_thread ON artifacts(thread_id, turn_seq DESC);
-- Pruning walks by age across every thread, exactly as it does for events.
CREATE INDEX artifacts_by_age ON artifacts(changed_at);

-- Expanding one collapsed trace asks for a single run's events. Without this
-- that is a scan of the thread's whole timeline with turn_seq filtered per row,
-- which grows with the conversation rather than with the run being read.
CREATE INDEX events_by_turn ON events(turn_seq, seq);

-- On what this migration does not backfill: pre-migration bot messages carry
-- turn_seq 0 and always will. The counters could be rebuilt from events, which
-- has held turn_seq and kind since 0001 - 0002 rebuilt its index for less - but
-- the link from a message to its run cannot be. Bracketing by turn_start and
-- turn_end is exactly the inference this column exists to replace: a bot may
-- post several times in a run, and a run killed with the process wrote no end.
-- Counters with no message to hang them off would buy nothing, so neither is
-- backfilled and older turns are permanently trace-less.
