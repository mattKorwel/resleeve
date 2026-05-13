-- Resleeve v1 auth schema: one users row holds the KEK two-wrap material.
-- See docs/design/round-2/10-auth-subsystem.md.

CREATE TABLE users (
    id                       TEXT PRIMARY KEY,
    email                    TEXT NOT NULL UNIQUE,
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL,

    -- Argon2id parameters used for all four derivations.
    argon2_memory_kib        INTEGER NOT NULL,
    argon2_time_iters        INTEGER NOT NULL,
    argon2_parallelism       INTEGER NOT NULL,

    -- Password verifier: distinct salt from password KEK so the verifier
    -- is safe to store without leaking the KEK-wrap key.
    password_verifier_salt   BLOB NOT NULL,
    password_verifier_hash   BLOB NOT NULL,

    -- KEK wrapped with key derived from password.
    password_kek_salt        BLOB NOT NULL,
    password_kek_nonce       BLOB NOT NULL,
    password_kek_ct          BLOB NOT NULL,

    -- Recovery key verifier (separate salt for the same reason).
    recovery_verifier_salt   BLOB NOT NULL,
    recovery_verifier_hash   BLOB NOT NULL,

    -- KEK wrapped with key derived from recovery key.
    recovery_kek_salt        BLOB NOT NULL,
    recovery_kek_nonce       BLOB NOT NULL,
    recovery_kek_ct          BLOB NOT NULL
);
