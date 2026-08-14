-- Where to reach the operator when no page is open.
--
-- A push subscription is a capability, not an identity: whoever holds the
-- endpoint can make that browser show a notification. There is one operator, so
-- there is no owner column - every row here is theirs - and the rows live in
-- the store rather than in a config file because they are created and destroyed
-- by the browser, not by a person editing anything.
--
-- The endpoint is the key. A browser that re-subscribes after clearing its site
-- data gets a new one and lands in a new row; the old row stays until the push
-- service answers 404 or 410 for it, which is the only authority on whether a
-- subscription still exists.
CREATE TABLE push_subs (
  endpoint   TEXT PRIMARY KEY,
  -- The subscription's public key and its auth secret, base64url exactly as the
  -- browser wrote them. Stored as text so what is in the row is what
  -- pushManager.subscribe produced, and a mismatch is readable rather than a
  -- decoding question.
  p256dh     TEXT    NOT NULL,
  auth       TEXT    NOT NULL,
  created_at INTEGER NOT NULL
) WITHOUT ROWID;

-- Oldest first, for the cap that keeps a browser which re-subscribes on every
-- load from growing this table without bound.
CREATE INDEX push_subs_by_age ON push_subs(created_at);
