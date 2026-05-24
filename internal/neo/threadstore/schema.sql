CREATE TABLE IF NOT EXISTS threads (
    id TEXT PRIMARY KEY,
    v INTEGER NOT NULL,
    created INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0,
    title TEXT NOT NULL DEFAULT '',
    agent_mode TEXT NOT NULL DEFAULT '',
    reasoning_effort TEXT NOT NULL DEFAULT '',
    creator_user_id TEXT NOT NULL DEFAULT '',
    raw_json BLOB NOT NULL,
    message_count INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_threads_updated_at ON threads(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_threads_title ON threads(title);
