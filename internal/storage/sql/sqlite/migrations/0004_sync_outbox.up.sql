-- v2 slice 2: outbox + sync cursor state.
-- See docs/design/round-4/02-cross-machine-sync.md §"Local outbox" and §"Sync protocol".

CREATE TABLE outbox (
    seq             INTEGER PRIMARY KEY AUTOINCREMENT,
    kind            TEXT    NOT NULL,                  -- 'sessions' | 'events' (slow tier; 'memory' joins in slice 3 SSE)
    key             TEXT    NOT NULL,                  -- backend key the upstream Put will use
    blob            BLOB    NOT NULL,                  -- opaque bytes (encryption layer lands in slice 2.5)
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT    NOT NULL,                  -- RFC 3339; rows with next_attempt_at <= now are eligible for drain
    last_error      TEXT,
    UNIQUE(kind, key)                                  -- idempotent enqueue: same row inserted twice is a no-op
);

CREATE INDEX outbox_by_attempt ON outbox(next_attempt_at);

CREATE TABLE sync_state (
    kind        TEXT    PRIMARY KEY,                   -- 'sessions' | 'events' | 'memory'
    cursor      TEXT    NOT NULL DEFAULT '',           -- last server cursor seen via /v2/sync/pull
    updated_at  TEXT    NOT NULL
);
