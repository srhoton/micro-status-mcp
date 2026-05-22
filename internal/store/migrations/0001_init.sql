CREATE TABLE IF NOT EXISTS sessions (
    repo             TEXT PRIMARY KEY,
    pane             TEXT NOT NULL,
    -- Stored as Unix epoch nanoseconds (signed 64-bit) for fast reads and
    -- native ordering. Convert to/from time.Time in store.go.
    registered_at_ns INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    from_repo    TEXT NOT NULL,
    to_repo      TEXT NOT NULL,
    subject      TEXT NOT NULL,
    body         TEXT NOT NULL,
    posted_at_ns INTEGER NOT NULL,
    -- NULL means unread. Stored as Unix epoch nanoseconds when set.
    -- No FK to sessions(repo): messages must survive after a recipient
    -- unregisters, and senders may never have registered at all.
    read_at_ns   INTEGER
);

-- Partial index for the common "show me my unread inbox" query:
--   WHERE to_repo = ? AND read_at_ns IS NULL ORDER BY posted_at_ns, id
CREATE INDEX IF NOT EXISTS idx_messages_unread
    ON messages(to_repo, posted_at_ns, id)
    WHERE read_at_ns IS NULL;

-- Full index covering the include_read=true path as well.
CREATE INDEX IF NOT EXISTS idx_messages_to_posted
    ON messages(to_repo, posted_at_ns, id);
