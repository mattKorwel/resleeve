-- Resleeve round 11a slice 1: multi-tenant schema foundation.
-- See docs/design/round-11/02-multi-tenant.md §"Core model" and
-- docs/design/round-11/03-schema-slice.md.
--
-- Three net-new server-side tables (only `resleeve serve` consults them):
--   * brains       — a named namespace = the unit of isolation AND of
--                    sharing. kind='personal' brains are auto-provisioned
--                    on register; kind='shared' brains gain members later.
--   * memberships  — the access edge: a row (brain_id, user_id) means the
--                    user may read+write that brain. NO role column yet
--                    (members read+write; the owner is brains.owner_user_id).
--                    A shared brain is simply one with count(members) > 1.
--   * credentials  — a way to prove you are a user: an SSH pubkey, an API
--                    key hash, or (later) an OIDC subject. Storage shape
--                    only in this slice — SSH/API verification lands in a
--                    later slice. Many users × many keys is just rows.

CREATE TABLE brains (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    kind           TEXT NOT NULL,          -- 'personal' | 'shared'
    owner_user_id  TEXT NOT NULL,          -- creator; can add/remove members
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    FOREIGN KEY (owner_user_id) REFERENCES server_users(id) ON DELETE CASCADE
);

CREATE INDEX brains_by_owner ON brains(owner_user_id);

CREATE TABLE memberships (
    brain_id   TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (brain_id, user_id),
    FOREIGN KEY (brain_id) REFERENCES brains(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id)  REFERENCES server_users(id) ON DELETE CASCADE
);

CREATE INDEX memberships_by_user ON memberships(user_id);

CREATE TABLE credentials (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL,
    kind                TEXT NOT NULL,      -- 'ssh' | 'api' | 'oidc'
    public_key_or_hash  TEXT NOT NULL,      -- SSH pubkey, API-key hash, or OIDC subject
    label               TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    expires_at          TEXT,               -- NULL = no expiry
    FOREIGN KEY (user_id) REFERENCES server_users(id) ON DELETE CASCADE
);

CREATE INDEX credentials_by_user ON credentials(user_id);
