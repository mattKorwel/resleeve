-- Resleeve v2 slice 3 (punch-list #3): identity-side schema for `resleeve serve`.
-- See docs/design/round-4/02-cross-machine-sync.md §"Identity" and §"Pairing flow".
--
-- Three tables live alongside the existing client-side `users` table.
-- They are server-side concepts (only `resleeve serve` consults them):
--   * server_users — one row per registered account; holds the Argon2id
--     verifier + the password-wrapped + recovery-wrapped KEK. The server
--     never sees the plaintext KEK; the verifier is independent salt so
--     leaking it does NOT leak the KEK-wrap derivation key.
--   * devices — one row per paired device. The device_token is the
--     bearer the device presents on every /v2/sync/* call; per-device so
--     a single compromise rotates without revoking the others.
--   * pairing_codes — short-lived (≤5 min) one-time codes used to bridge
--     a new device onto an existing account. Holds the KEK wrapped under
--     a key derived from the (small) code itself, plus a separate
--     verifier so a brute-force attempt against the code is bounded by
--     Argon2id cost AND the 5-minute TTL.

CREATE TABLE server_users (
    id                       TEXT PRIMARY KEY,
    email                    TEXT NOT NULL UNIQUE,
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL,

    -- Argon2id parameters used for the verifier and the password/recovery KEK wraps.
    argon2_memory_kib        INTEGER NOT NULL,
    argon2_time_iters        INTEGER NOT NULL,
    argon2_parallelism       INTEGER NOT NULL,

    -- Password verifier (salt independent of password-KEK salt).
    password_verifier_salt   BLOB NOT NULL,
    password_verifier_hash   BLOB NOT NULL,

    -- KEK wrapped with key derived from password.
    password_kek_salt        BLOB NOT NULL,
    password_kek_nonce       BLOB NOT NULL,
    password_kek_ct          BLOB NOT NULL,

    -- Recovery key verifier (independent salt).
    recovery_verifier_salt   BLOB NOT NULL,
    recovery_verifier_hash   BLOB NOT NULL,

    -- KEK wrapped with key derived from recovery key.
    recovery_kek_salt        BLOB NOT NULL,
    recovery_kek_nonce       BLOB NOT NULL,
    recovery_kek_ct          BLOB NOT NULL
);

CREATE TABLE devices (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    device_token    TEXT NOT NULL UNIQUE,
    created_at      TEXT NOT NULL,
    last_seen_at    TEXT NOT NULL,
    FOREIGN KEY (user_id) REFERENCES server_users(id) ON DELETE CASCADE
);

CREATE INDEX devices_by_user ON devices(user_id);

CREATE TABLE pairing_codes (
    code_id          TEXT PRIMARY KEY,           -- public id of the pairing offer
    user_id          TEXT NOT NULL,              -- which account this code unlocks

    -- Argon2id parameters used for the code verifier + code-KEK wrap.
    argon2_memory_kib INTEGER NOT NULL,
    argon2_time_iters INTEGER NOT NULL,
    argon2_parallelism INTEGER NOT NULL,

    -- Verifier proves the claimant typed the right pair code. Independent
    -- salt from the code-KEK wrap salt — same rationale as users above.
    code_verifier_salt BLOB NOT NULL,
    code_verifier_hash BLOB NOT NULL,

    -- KEK wrapped with key derived from the pair code itself. Whoever
    -- has the code can both prove it (verifier) and decrypt the KEK.
    code_kek_salt    BLOB NOT NULL,
    code_kek_nonce   BLOB NOT NULL,
    code_kek_ct      BLOB NOT NULL,

    created_at       TEXT NOT NULL,
    expires_at       TEXT NOT NULL,              -- ~5 minutes after created_at
    claimed_at       TEXT,                       -- non-NULL once consumed; codes are one-shot
    FOREIGN KEY (user_id) REFERENCES server_users(id) ON DELETE CASCADE
);

CREATE INDEX pairing_codes_by_expiry ON pairing_codes(expires_at);
