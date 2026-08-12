-- Reclaiming space has to be decided while the database is empty: auto_vacuum
-- can only be changed here or by a full VACUUM. Prune deletes timeline events
-- by age, and without this the file would only ever grow - which would undercut
-- the whole reason for pruning.
PRAGMA auto_vacuum = INCREMENTAL;

-- One global monotonic sequence, allocated inside every write transaction.
-- Messages and timeline events must interleave in one resumable stream, so a
-- per-table AUTOINCREMENT would give a client two numbers to hold and no way to
-- order them against each other. UPDATE ... RETURNING makes allocating a number
-- and writing the row one atomic step.
CREATE TABLE cursor (
  id  INTEGER PRIMARY KEY CHECK (id = 1),
  seq INTEGER NOT NULL
);
INSERT INTO cursor (id, seq) VALUES (1, 0);

CREATE TABLE actors (
  id         TEXT PRIMARY KEY,           -- 'human:operator' | 'bot:jane'
  kind       TEXT NOT NULL,              -- 'human' | 'bot'
  name       TEXT NOT NULL,
  role       TEXT NOT NULL DEFAULT '',
  -- present is whether the bot is running in the fleet right now. It is reset
  -- on startup rather than trusted from the last run, because a process that
  -- was killed had no chance to clear it.
  present    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE threads (
  id          TEXT PRIMARY KEY,          -- 't_' + 16 hex
  title       TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL REFERENCES actors(id),
  -- The denormalised tail, written in the same transaction as the message. The
  -- inbox is the most frequent query in the product and it must not fan out to
  -- a subquery per thread just to draw a preview line.
  last_seq    INTEGER NOT NULL DEFAULT 0,
  last_at     INTEGER NOT NULL,
  last_author TEXT    NOT NULL DEFAULT '',
  last_text   TEXT    NOT NULL DEFAULT '',
  state       TEXT    NOT NULL DEFAULT 'idle',
  archived_at INTEGER,
  -- changed_seq is the sequence of the last change of any kind to this thread:
  -- a message, a rename, an archive, a participant, a turn starting or ending.
  -- last_seq only moves for messages, so without this a client that slept
  -- through a rename would never hear about it - every one of those changes
  -- publishes live and would otherwise leave nothing behind to replay.
  changed_seq INTEGER NOT NULL DEFAULT 0
);
-- The inbox orders by changed_seq, not last_seq: a thread someone just opened
-- has no messages yet and would otherwise sort to the bottom of the list it was
-- created from. There is deliberately no index on state - the inbox filters on
-- the effective state, which is a CASE over the turns table, so an index on the
-- stored column could never serve it and would only cost every write.
CREATE INDEX threads_inbox ON threads(archived_at, changed_seq DESC);

-- Tombstones, so a client that was away learns a thread is gone rather than
-- rendering one that no longer exists. One row per former participant, because
-- the participants rows are cascaded away with the thread and there would
-- otherwise be no way to tell who was entitled to hear about the deletion.
CREATE TABLE deleted_threads (
  seq       INTEGER NOT NULL,
  thread_id TEXT    NOT NULL,
  actor_id  TEXT    NOT NULL,
  PRIMARY KEY (seq, actor_id)
) WITHOUT ROWID;
CREATE INDEX deleted_threads_by_actor ON deleted_threads(actor_id, seq);

-- Participation is the whole authorization boundary. There is no channel above
-- a thread, so this table is the only thing that decides who may read or write.
CREATE TABLE participants (
  thread_id TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  actor_id  TEXT    NOT NULL REFERENCES actors(id),
  added_at  INTEGER NOT NULL,
  added_by  TEXT    NOT NULL,
  PRIMARY KEY (thread_id, actor_id)
) WITHOUT ROWID;
CREATE INDEX participants_by_actor ON participants(actor_id, thread_id);

CREATE TABLE messages (
  seq        INTEGER PRIMARY KEY,        -- from cursor, never AUTOINCREMENT
  thread_id  TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  author_id  TEXT    NOT NULL,
  body       TEXT    NOT NULL,
  kind       TEXT    NOT NULL DEFAULT 'message',
  -- Space-padded actor ids, so a LIKE '% bot:jane %' cannot match a prefix of
  -- another name. Mentions are a convenience for the UI; what actually wakes a
  -- bot is participation, not this column.
  mentions   TEXT    NOT NULL DEFAULT '',
  await      INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
-- author_id is in the index so the inbox's unread count is index-only. Without
-- it every candidate row costs a table lookup, and for a reader who has never
-- marked anything read that is the thread's entire history, on every draw.
CREATE INDEX messages_by_thread ON messages(thread_id, seq, author_id);

-- The agent timeline. One row per uisession.Event, payload stored as the JSON
-- the browser already decodes, so there is no second wire format to keep in
-- step with protocol.ts.
CREATE TABLE events (
  seq        INTEGER PRIMARY KEY,
  thread_id  TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  actor_id   TEXT    NOT NULL,
  turn_seq   INTEGER NOT NULL,
  kind       TEXT    NOT NULL,
  payload    BLOB    NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX events_by_thread ON events(thread_id, seq);
-- Pruning walks by age across every thread, which the index above does not serve.
CREATE INDEX events_by_age ON events(created_at);

-- Oversized tool results, exactly as uisession/journal.go splits them: the
-- timeline keeps the head, the whole body is fetched when someone expands the
-- call.
CREATE TABLE blobs (
  seq  INTEGER PRIMARY KEY REFERENCES events(seq) ON DELETE CASCADE,
  body BLOB NOT NULL
);

-- An attachment belongs to a thread. The bytes on disk are content-addressed
-- and shared, but the row is not: the same image uploaded into two threads is
-- two records, because thread_id is what the participation check reads, and one
-- row would mean the second uploader could not see their own file.
CREATE TABLE attachments (
  id         TEXT    PRIMARY KEY,
  thread_id  TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  filename   TEXT    NOT NULL,
  mime       TEXT    NOT NULL,
  size       INTEGER NOT NULL,
  sha256     TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (thread_id, sha256)
);
CREATE INDEX attachments_by_sha ON attachments(sha256);

-- Which messages carry which files. It is a relation rather than a column on
-- attachments because one upload may legitimately ride on several messages, and
-- a single message_seq column silently moved the file off the earlier one.
CREATE TABLE message_attachments (
  message_seq   INTEGER NOT NULL REFERENCES messages(seq) ON DELETE CASCADE,
  attachment_id TEXT    NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
  PRIMARY KEY (message_seq, attachment_id)
) WITHOUT ROWID;
CREATE INDEX message_attachments_by_attachment ON message_attachments(attachment_id);

CREATE TABLE read_state (
  actor_id      TEXT    NOT NULL,
  thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  last_read_seq INTEGER NOT NULL,
  PRIMARY KEY (actor_id, thread_id)
) WITHOUT ROWID;

-- Live turn tracking, so "amiran is working" survives a page reload and a
-- browser that was closed. A transient typing ping cannot.
CREATE TABLE turns (
  turn_seq      INTEGER PRIMARY KEY,
  thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  actor_id      TEXT    NOT NULL,
  started_at    INTEGER NOT NULL,
  ended_at      INTEGER,
  in_tokens     INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  out_tokens    INTEGER NOT NULL DEFAULT 0,
  calls         INTEGER NOT NULL DEFAULT 0,
  uncounted     INTEGER NOT NULL DEFAULT 0,
  model         TEXT    NOT NULL DEFAULT '',
  error         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX turns_live ON turns(thread_id) WHERE ended_at IS NULL;
CREATE INDEX turns_by_thread ON turns(thread_id, turn_seq);
