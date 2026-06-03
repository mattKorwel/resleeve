-- Resleeve v1 memory module: scopes, plans (named slots), learnings
-- (append-only with soft-supersede chains).
-- See docs/design/round-3/01-memory-module.md.

CREATE TABLE scopes (
    path             TEXT PRIMARY KEY,
    kind             TEXT NOT NULL DEFAULT '',
    title            TEXT NOT NULL DEFAULT '',
    description      TEXT NOT NULL DEFAULT '',
    cwd              TEXT NOT NULL DEFAULT '',
    do_not_inherit   INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE TABLE plans (
    scope       TEXT NOT NULL,
    name        TEXT NOT NULL DEFAULT '_default',
    content     TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (scope, name),
    FOREIGN KEY (scope) REFERENCES scopes(path) ON DELETE CASCADE
);

CREATE TABLE learnings (
    id              TEXT PRIMARY KEY,
    scope           TEXT NOT NULL,
    content         TEXT NOT NULL,
    supersedes_id   TEXT,
    created_at      TEXT NOT NULL,
    FOREIGN KEY (scope) REFERENCES scopes(path) ON DELETE CASCADE,
    FOREIGN KEY (supersedes_id) REFERENCES learnings(id)
);

CREATE INDEX learnings_by_scope ON learnings(scope, created_at);
CREATE INDEX learnings_by_supersedes ON learnings(supersedes_id);
