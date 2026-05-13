package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

type userStore struct{ db *sql.DB }

const userColumns = `id, email, created_at, updated_at,
	argon2_memory_kib, argon2_time_iters, argon2_parallelism,
	password_verifier_salt, password_verifier_hash,
	password_kek_salt, password_kek_nonce, password_kek_ct,
	recovery_verifier_salt, recovery_verifier_hash,
	recovery_kek_salt, recovery_kek_nonce, recovery_kek_ct`

func (s *userStore) Create(ctx context.Context, u *auth.User) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users (`+userColumns+`)
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

func (s *userStore) GetByID(ctx context.Context, id string) (*auth.User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	return s.scanUser(row)
}

func (s *userStore) GetByEmail(ctx context.Context, email string) (*auth.User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, email)
	return s.scanUser(row)
}

func (s *userStore) UpdateAuth(ctx context.Context, u *auth.User) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET
		updated_at = ?,
		argon2_memory_kib = ?, argon2_time_iters = ?, argon2_parallelism = ?,
		password_verifier_salt = ?, password_verifier_hash = ?,
		password_kek_salt = ?, password_kek_nonce = ?, password_kek_ct = ?,
		recovery_verifier_salt = ?, recovery_verifier_hash = ?,
		recovery_kek_salt = ?, recovery_kek_nonce = ?, recovery_kek_ct = ?
		WHERE id = ?`,
		u.UpdatedAt.UTC().Format(time.RFC3339Nano),
		u.Params.MemoryKiB, u.Params.TimeIters, u.Params.Parallelism,
		u.PasswordVerifier.Salt, u.PasswordVerifier.Hash,
		u.PasswordKEK.Salt, u.PasswordKEK.Nonce, u.PasswordKEK.Ciphertext,
		u.RecoveryVerifier.Salt, u.RecoveryVerifier.Hash,
		u.RecoveryKEK.Salt, u.RecoveryKEK.Nonce, u.RecoveryKEK.Ciphertext,
		u.ID,
	)
	return err
}

func (s *userStore) scanUser(row *sql.Row) (*auth.User, error) {
	var u auth.User
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
	// Propagate top-level Params onto the wrapped KEKs so Unwrap works.
	u.PasswordKEK.Params = u.Params
	u.RecoveryKEK.Params = u.Params
	return &u, nil
}
