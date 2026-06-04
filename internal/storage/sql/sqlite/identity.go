package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// pairingTimeLayout is a fixed-width RFC 3339 layout with full
// nanosecond precision (always 9 fractional digits). pairing_codes.expires_at
// is lexicographically compared by Claim (`WHERE expires_at > ?`) and
// SweepExpired (`WHERE expires_at <= ?`), so the stored timestamp MUST
// be width-stable. See outboxTimeLayout in sync.go for the longer
// rationale — same bug class. created_at and claimed_at share the
// helper for consistency even though they aren't currently lex-compared.
const pairingTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatPairingTime(t time.Time) string {
	return t.UTC().Format(pairingTimeLayout)
}

// parsePairingTime accepts either the new fixed-width layout or the
// legacy time.RFC3339Nano so rows written by older daemon binaries
// still parse cleanly.
func parsePairingTime(s string) (time.Time, error) {
	if t, err := time.Parse(pairingTimeLayout, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// serverUserStore implements rsql.ServerUserStore against the
// server-side `server_users` table introduced in migration 0005. It is
// intentionally distinct from userStore (which serves the client-side
// `users` table) because the two run in different processes and we do
// NOT want a single Store accidentally mixing the two roles.
type serverUserStore struct{ db *sql.DB }

const serverUserColumns = `id, email, created_at, updated_at,
	argon2_memory_kib, argon2_time_iters, argon2_parallelism,
	password_verifier_salt, password_verifier_hash,
	password_kek_salt, password_kek_nonce, password_kek_ct,
	recovery_verifier_salt, recovery_verifier_hash,
	recovery_kek_salt, recovery_kek_nonce, recovery_kek_ct`

func (s *serverUserStore) Create(ctx context.Context, u *rsql.ServerUser) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO server_users (`+serverUserColumns+`)
		VALUES (?,?,?,?, ?,?,?, ?,?, ?,?,?, ?,?, ?,?,?)`,
		u.ID, u.Email,
		u.CreatedAt.UTC().Format(time.RFC3339Nano),
		u.UpdatedAt.UTC().Format(time.RFC3339Nano),
		u.Params.MemoryKiB, u.Params.TimeIters, u.Params.Parallelism,
		u.PasswordVerifier.Salt, u.PasswordVerifier.Hash,
		u.PasswordKEK.Salt, u.PasswordKEK.Nonce, u.PasswordKEK.Ciphertext,
		u.RecoveryVerifier.Salt, u.RecoveryVerifier.Hash,
		u.RecoveryKEK.Salt, u.RecoveryKEK.Nonce, u.RecoveryKEK.Ciphertext,
	)
	return err
}

func (s *serverUserStore) GetByID(ctx context.Context, id string) (*rsql.ServerUser, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+serverUserColumns+` FROM server_users WHERE id = ?`, id)
	return s.scan(row)
}

func (s *serverUserStore) GetByEmail(ctx context.Context, email string) (*rsql.ServerUser, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+serverUserColumns+` FROM server_users WHERE email = ?`, email)
	return s.scan(row)
}

func (s *serverUserStore) scan(row *sql.Row) (*rsql.ServerUser, error) {
	var u rsql.ServerUser
	var created, updated string
	if err := row.Scan(
		&u.ID, &u.Email, &created, &updated,
		&u.Params.MemoryKiB, &u.Params.TimeIters, &u.Params.Parallelism,
		&u.PasswordVerifier.Salt, &u.PasswordVerifier.Hash,
		&u.PasswordKEK.Salt, &u.PasswordKEK.Nonce, &u.PasswordKEK.Ciphertext,
		&u.RecoveryVerifier.Salt, &u.RecoveryVerifier.Hash,
		&u.RecoveryKEK.Salt, &u.RecoveryKEK.Nonce, &u.RecoveryKEK.Ciphertext,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
		u.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, updated); err == nil {
		u.UpdatedAt = t
	}
	// Propagate Params onto the wrapped KEKs so Unwrap works directly.
	u.PasswordKEK.Params = u.Params
	u.RecoveryKEK.Params = u.Params
	return &u, nil
}

// deviceStore implements rsql.DeviceStore against the `devices` table.
type deviceStore struct{ db *sql.DB }

func (s *deviceStore) Create(ctx context.Context, d *rsql.Device) error {
	var expires sql.NullString
	if d.ExpiresAt != nil {
		expires = sql.NullString{Valid: true, String: d.ExpiresAt.UTC().Format(time.RFC3339Nano)}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO devices
		(id, user_id, name, device_token, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.UserID, d.Name, d.DeviceToken,
		d.CreatedAt.UTC().Format(time.RFC3339Nano),
		d.LastSeenAt.UTC().Format(time.RFC3339Nano),
		expires,
	)
	return err
}

func (s *deviceStore) GetByToken(ctx context.Context, token string) (*rsql.Device, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, device_token, created_at, last_seen_at, expires_at
		   FROM devices WHERE device_token = ?`, token)
	return s.scanDevice(row)
}

func (s *deviceStore) ListByUser(ctx context.Context, userID string) ([]*rsql.Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, name, device_token, created_at, last_seen_at, expires_at
		   FROM devices WHERE user_id = ? ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*rsql.Device
	for rows.Next() {
		var d rsql.Device
		var created, lastSeen string
		var expires sql.NullString
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.DeviceToken, &created, &lastSeen, &expires); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
			d.CreatedAt = t
		}
		if t, err := time.Parse(time.RFC3339Nano, lastSeen); err == nil {
			d.LastSeenAt = t
		}
		if expires.Valid {
			if t, err := time.Parse(time.RFC3339Nano, expires.String); err == nil {
				d.ExpiresAt = &t
			}
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

func (s *deviceStore) TouchLastSeen(ctx context.Context, deviceID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), deviceID)
	return err
}

func (s *deviceStore) Delete(ctx context.Context, deviceID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, deviceID)
	return err
}

func (s *deviceStore) scanDevice(row *sql.Row) (*rsql.Device, error) {
	var d rsql.Device
	var created, lastSeen string
	var expires sql.NullString
	if err := row.Scan(&d.ID, &d.UserID, &d.Name, &d.DeviceToken, &created, &lastSeen, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
		d.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, lastSeen); err == nil {
		d.LastSeenAt = t
	}
	if expires.Valid {
		if t, err := time.Parse(time.RFC3339Nano, expires.String); err == nil {
			d.ExpiresAt = &t
		}
	}
	return &d, nil
}

// serveMetaStore implements rsql.ServeMetaStore against the `serve_meta`
// table (migration 0006). One row per persistent process secret. v1
// holds exactly one key: "login_challenge_key" (sec-M1). See the migration
// header for why the key needs to outlive a restart.
type serveMetaStore struct{ db *sql.DB }

func (s *serveMetaStore) GetOrCreateBytes(ctx context.Context, key string, genFn func() ([]byte, error)) ([]byte, error) {
	// Fast path: row already exists.
	var val []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM serve_meta WHERE key = ?`, key).Scan(&val)
	if err == nil {
		return val, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// First-time generate + insert. INSERT OR IGNORE makes the lost-race
	// case a no-op so two concurrent first-callers both succeed; the
	// SELECT after re-reads whichever value won.
	gen, err := genFn()
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO serve_meta (key, value) VALUES (?, ?)`,
		key, gen); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM serve_meta WHERE key = ?`, key).Scan(&val); err != nil {
		return nil, err
	}
	return val, nil
}

// pairingStore implements rsql.PairingStore against the `pairing_codes` table.
type pairingStore struct{ db *sql.DB }

const pairingCols = `code_id, user_id,
	argon2_memory_kib, argon2_time_iters, argon2_parallelism,
	code_verifier_salt, code_verifier_hash,
	code_kek_salt, code_kek_nonce, code_kek_ct,
	created_at, expires_at, claimed_at`

func (s *pairingStore) Create(ctx context.Context, c *rsql.PairingCode) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pairing_codes (`+pairingCols+`)
		 VALUES (?,?, ?,?,?, ?,?, ?,?,?, ?, ?, NULL)`,
		c.CodeID, c.UserID,
		c.Params.MemoryKiB, c.Params.TimeIters, c.Params.Parallelism,
		c.Verifier.Salt, c.Verifier.Hash,
		c.WrappedK.Salt, c.WrappedK.Nonce, c.WrappedK.Ciphertext,
		formatPairingTime(c.CreatedAt),
		formatPairingTime(c.ExpiresAt),
	)
	return err
}

func (s *pairingStore) Get(ctx context.Context, codeID string) (*rsql.PairingCode, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pairingCols+` FROM pairing_codes WHERE code_id = ?`, codeID)
	var c rsql.PairingCode
	var created, expires string
	var claimed sql.NullString
	if err := row.Scan(
		&c.CodeID, &c.UserID,
		&c.Params.MemoryKiB, &c.Params.TimeIters, &c.Params.Parallelism,
		&c.Verifier.Salt, &c.Verifier.Hash,
		&c.WrappedK.Salt, &c.WrappedK.Nonce, &c.WrappedK.Ciphertext,
		&created, &expires, &claimed,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	if t, err := parsePairingTime(created); err == nil {
		c.CreatedAt = t
	}
	if t, err := parsePairingTime(expires); err == nil {
		c.ExpiresAt = t
	}
	if claimed.Valid {
		if t, err := parsePairingTime(claimed.String); err == nil {
			c.ClaimedAt = &t
		}
	}
	c.WrappedK.Params = c.Params
	return &c, nil
}

func (s *pairingStore) Claim(ctx context.Context, codeID string, now time.Time) error {
	// Atomic compare-and-swap: only an unclaimed, unexpired code transitions.
	res, err := s.db.ExecContext(ctx,
		`UPDATE pairing_codes SET claimed_at = ?
		   WHERE code_id = ? AND claimed_at IS NULL AND expires_at > ?`,
		formatPairingTime(now), codeID,
		formatPairingTime(now),
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return rsql.ErrNotFound
	}
	return nil
}

func (s *pairingStore) Delete(ctx context.Context, codeID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pairing_codes WHERE code_id = ?`, codeID)
	return err
}

func (s *pairingStore) SweepExpired(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pairing_codes WHERE expires_at <= ?`,
		formatPairingTime(now))
	return err
}

// Compile-time assertions that our impls satisfy the contracts.
var (
	_ rsql.ServerUserStore = (*serverUserStore)(nil)
	_ rsql.DeviceStore     = (*deviceStore)(nil)
	_ rsql.PairingStore    = (*pairingStore)(nil)
	_ rsql.ServeMetaStore  = (*serveMetaStore)(nil)
)
