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
  archived_at INTEGER
);
CREATE INDEX threads_inbox ON threads(archived_at, last_seq DESC);
CREATE INDEX threads_state ON threads(state, last_seq DESC) WHERE archived_at IS NULL;

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
CREATE INDEX messages_by_thread ON messages(thread_id, seq);

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
CREATE INDEX events_by_turn   ON events(turn_seq, seq);
-- Pruning walks by age across every thread, which neither index above serves.
CREATE INDEX events_by_age ON events(created_at);

-- Oversized tool results, exactly as uisession/journal.go splits them: the
-- timeline keeps the head, the whole body is fetched when someone expands the
-- call.
CREATE TABLE blobs (
  seq  INTEGER PRIMARY KEY REFERENCES events(seq) ON DELETE CASCADE,
  body BLOB NOT NULL
);

CREATE TABLE attachments (
  id          TEXT    PRIMARY KEY,
  thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  -- NULL until the message carrying it is written, so an upload that is never
  -- sent is collectable rather than orphaned onto some other message.
  message_seq INTEGER REFERENCES messages(seq) ON DELETE CASCADE,
  filename    TEXT    NOT NULL,
  mime        TEXT    NOT NULL,
  size        INTEGER NOT NULL,
  sha256      TEXT    NOT NULL,
  created_at  INTEGER NOT NULL
);
CREATE INDEX attachments_by_message ON attachments(message_seq);
CREATE INDEX attachments_by_sha ON attachments(sha256);

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
