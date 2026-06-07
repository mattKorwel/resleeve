-- Resleeve round 12 Part A slice 1: server-at-rest envelope encryption.
-- See docs/design/round-12/02-server-at-rest-slice.md.
--
-- Two net-new server-side pieces (only `resleeve serve` consults them):
--   * brain_keys              — one wrapped Data Encryption Key (DEK) per
--                               brain. The DEK is AES-256-GCM-encrypted
--                               under the operator's master key (envelope
--                               encryption), so the stored bytes are
--                               useless without the master key. ON DELETE
--                               CASCADE: dropping a brain drops its key.
--   * brains.encryption_policy — per-brain policy the encryption matrix
--                               reads: 'server-side' (default, the only
--                               value exercised this slice) or 'e2e'
--                               (end-to-end / zero-knowledge, a later
--                               slice). Stored now; not branched on yet.

CREATE TABLE brain_keys (
    brain_id    TEXT PRIMARY KEY,
    wrapped_dek BLOB NOT NULL,         -- DEK encrypted under the master key (nonce||ct+tag)
    created_at  TEXT NOT NULL,
    FOREIGN KEY (brain_id) REFERENCES brains(id) ON DELETE CASCADE
);

ALTER TABLE brains
    ADD COLUMN encryption_policy TEXT NOT NULL DEFAULT 'server-side';  -- 'server-side' | 'e2e'
