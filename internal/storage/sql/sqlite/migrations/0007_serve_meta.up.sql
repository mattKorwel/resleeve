-- sec-M1: persist the login-challenge HMAC key across `resleeve serve`
-- restarts so the synthetic salt returned for unknown-email
-- login-challenge probes is stable across reboots. Previously the key
-- was generated fresh in serve.New on every process start, which gave a
-- passive observer a "did the server restart?" oracle: real account
-- salts (stored in server_users.password_verifier_salt) are stable
-- forever, while decoy salts churn each restart. After this migration,
-- the key is generated on first boot and never rotated.
--
-- sec-M5: also add devices.expires_at as a server-side TTL on per-device
-- bearer tokens. Ephemeral devices minted during `resleeve pair invite`
-- request a 60-second TTL so a failed best-effort revoke (the existing
-- /v2/auth/logout call in pair.go) no longer leaves a year-long bearer
-- lying around. NULL = no expiry, which matches the v1 behavior for
-- existing rows (backfilled by the column-add).
--
-- serve_meta is a single-row key/value table for server-side process
-- secrets that need to outlive a restart but don't belong with any one
-- user. Keep this table narrow on purpose: it's a footgun magnet for
-- ad-hoc config.

CREATE TABLE serve_meta (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);

ALTER TABLE devices ADD COLUMN expires_at TEXT;
