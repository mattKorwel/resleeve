-- Resleeve v1 initial schema.
-- Portable ANSI SQL only: TEXT/INTEGER/BLOB types, no backend-specific
-- features. Postgres / Spanner-PG / AlloyDB implementations share this
-- migration as-is. See docs/design/round-2/11-storage-backends.md.

CREATE TABLE sessions (
    id            TEXT PRIMARY KEY,
    scope         TEXT NOT NULL,
    agent_name    TEXT NOT NULL,
    cli           TEXT NOT NULL,
    cli_version   TEXT NOT NULL,
    cwd           TEXT NOT NULL,
    git_branch    TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    started_at    TEXT NOT NULL,
    ended_at      TEXT,
    event_count   INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL,
    exit_status   INTEGER
);

CREATE INDEX sessions_by_slot   ON sessions(scope, agent_name, started_at);
CREATE INDEX sessions_by_status ON sessions(status, started_at);

CREATE TABLE events (
    event_uuid             TEXT NOT NULL,
    session_id             TEXT NOT NULL,
    seq                    INTEGER NOT NULL,
    turn_id                TEXT,
    parent_event_uuid      TEXT,
    ts                     TEXT NOT NULL,
    kind                   TEXT NOT NULL,
    schema_version         INTEGER NOT NULL,
    content                TEXT NOT NULL,
    vendor_name            TEXT NOT NULL,
    vendor_version         TEXT NOT NULL,
    vendor_native_payload  BLOB,
    PRIMARY KEY (session_id, event_uuid),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX events_by_seq  ON events(session_id, seq);
CREATE INDEX events_by_turn ON events(session_id, turn_id);
CREATE INDEX events_by_kind ON events(kind);

CREATE TABLE slots (
    scope              TEXT NOT NULL,
    agent_name         TEXT NOT NULL,
    last_heartbeat_at  TEXT NOT NULL,
    current_session_id TEXT,
    PRIMARY KEY (scope, agent_name)
);
