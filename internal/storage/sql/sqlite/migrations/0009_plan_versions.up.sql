-- Resleeve round 12 Part B: append-only plan versioning + provenance.
-- See docs/design/round-12/00-encryption-and-versioning.md §"Part B" and
-- docs/design/round-12/01-plan-versioning-slice.md.
--
-- GREEN FIELD: resleeve is pre-release. This drops the mutate-in-place
-- `plans` table and replaces it with an append-only `plan_versions` log.
-- No data migration — old plan rows are not preserved (per the round-12B
-- slice's explicit "no back-compat" constraint).
--
-- Model:
--   * Write = append, never mutate. Each update inserts an immutable row
--     keyed (scope, name, version) with content + author + parent_version.
--   * Read = materialized HEAD = the row with max(version) for a
--     (scope, name).
--   * Optimistic concurrency lives in the store layer: a write carries the
--     base_version it derived from; base_version != HEAD => conflict.

DROP TABLE IF EXISTS plans;

CREATE TABLE plan_versions (
    scope           TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '_default',
    version         INTEGER NOT NULL,            -- 1-based, monotonic per (scope, name)
    content         TEXT NOT NULL,
    author_user_id  TEXT NOT NULL DEFAULT '',    -- provenance; '' = unknown/local
    parent_version  INTEGER NOT NULL DEFAULT 0,  -- version this was derived from; 0 = first
    created_at      TEXT NOT NULL,
    PRIMARY KEY (scope, name, version),
    FOREIGN KEY (scope) REFERENCES scopes(path) ON DELETE CASCADE
);

-- HEAD lookup = max(version) per (scope, name); this index makes both the
-- HEAD scan and the per-slot history walk cheap.
CREATE INDEX plan_versions_head ON plan_versions(scope, name, version);

-- Provenance: add author_user_id to learnings (append-only already).
ALTER TABLE learnings ADD COLUMN author_user_id TEXT NOT NULL DEFAULT '';
