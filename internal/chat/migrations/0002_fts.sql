-- Full-text search over messages. This is the direct replacement for the
-- ambient channel buffer the Mattermost transport fed a bot as turn context:
-- with participation as the authorization boundary there is no stream a bot is
-- adjacent to but not in, so what it could once half-see it now has to look up
-- on purpose - scoped to its own threads, and by content rather than by a
-- twenty-minute ring.
CREATE VIRTUAL TABLE messages_fts USING fts5(
  body,
  content='messages',
  content_rowid='seq'
);

CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, body) VALUES (new.seq, new.body);
END;

CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.seq, old.body);
END;

CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.seq, old.body);
  INSERT INTO messages_fts(rowid, body) VALUES (new.seq, new.body);
END;
