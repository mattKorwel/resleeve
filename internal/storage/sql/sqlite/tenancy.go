package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// brainStore implements rsql.BrainStore against the `brains` table
// (migration 0008). Server-side only; the client daemon never reads it.
type brainStore struct{ db *sql.DB }

const brainColumns = `id, name, kind, owner_user_id, created_at, updated_at`

func (s *brainStore) Create(ctx context.Context, b *rsql.Brain) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO brains (`+brainColumns+`) VALUES (?,?,?,?,?,?)`,
		b.ID, b.Name, string(b.Kind), b.OwnerUserID,
		b.CreatedAt.UTC().Format(time.RFC3339Nano),
		b.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *brainStore) Get(ctx context.Context, id string) (*rsql.Brain, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+brainColumns+` FROM brains WHERE id = ?`, id)
	var b rsql.Brain
	var kind, created, updated string
	if err := row.Scan(&b.ID, &b.Name, &kind, &b.OwnerUserID, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	b.Kind = rsql.BrainKind(kind)
	if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
		b.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, updated); err == nil {
		b.UpdatedAt = t
	}
	return &b, nil
}

func (s *brainStore) ListByOwner(ctx context.Context, userID string) ([]*rsql.Brain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+brainColumns+` FROM brains WHERE owner_user_id = ? ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBrains(rows)
}

func (s *brainStore) ListForUser(ctx context.Context, userID string) ([]*rsql.Brain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+prefixedBrainColumns+`
		   FROM brains b
		   JOIN memberships m ON m.brain_id = b.id
		  WHERE m.user_id = ?
		  ORDER BY b.created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBrains(rows)
}

// prefixedBrainColumns is brainColumns qualified with the `b` alias for
// the membership join in ListForUser.
const prefixedBrainColumns = `b.id, b.name, b.kind, b.owner_user_id, b.created_at, b.updated_at`

func scanBrains(rows *sql.Rows) ([]*rsql.Brain, error) {
	var out []*rsql.Brain
	for rows.Next() {
		var b rsql.Brain
		var kind, created, updated string
		if err := rows.Scan(&b.ID, &b.Name, &kind, &b.OwnerUserID, &created, &updated); err != nil {
			return nil, err
		}
		b.Kind = rsql.BrainKind(kind)
		if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
			b.CreatedAt = t
		}
		if t, err := time.Parse(time.RFC3339Nano, updated); err == nil {
			b.UpdatedAt = t
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// membershipStore implements rsql.MembershipStore against the
// `memberships` table (migration 0008).
type membershipStore struct{ db *sql.DB }

func (s *membershipStore) Add(ctx context.Context, m *rsql.Membership) error {
	// INSERT OR IGNORE: a duplicate (brain_id, user_id) is a silent
	// no-op so re-adding an existing member doesn't error.
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO memberships (brain_id, user_id, created_at) VALUES (?,?,?)`,
		m.BrainID, m.UserID, m.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *membershipStore) Remove(ctx context.Context, brainID, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM memberships WHERE brain_id = ? AND user_id = ?`, brainID, userID)
	return err
}

func (s *membershipStore) ListBrains(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT brain_id FROM memberships WHERE user_id = ? ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIDs(rows)
}

func (s *membershipStore) ListMembers(ctx context.Context, brainID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id FROM memberships WHERE brain_id = ? ORDER BY created_at ASC`, brainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIDs(rows)
}

func (s *membershipStore) IsMember(ctx context.Context, userID, brainID string) (bool, error) {
	var dummy int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM memberships WHERE brain_id = ? AND user_id = ?`, brainID, userID).Scan(&dummy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// credentialStore implements rsql.CredentialStore against the
// `credentials` table (migration 0008). Storage shape only in this slice;
// SSH/API verification lands later.
type credentialStore struct{ db *sql.DB }

const credentialColumns = `id, user_id, kind, public_key_or_hash, label, created_at, expires_at`

func (s *credentialStore) Add(ctx context.Context, c *rsql.Credential) error {
	var expires sql.NullString
	if c.ExpiresAt != nil {
		expires = sql.NullString{Valid: true, String: c.ExpiresAt.UTC().Format(time.RFC3339Nano)}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials (`+credentialColumns+`) VALUES (?,?,?,?,?,?,?)`,
		c.ID, c.UserID, string(c.Kind), c.PublicKeyOrHash, c.Label,
		c.CreatedAt.UTC().Format(time.RFC3339Nano), expires)
	return err
}

func (s *credentialStore) Get(ctx context.Context, id string) (*rsql.Credential, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+credentialColumns+` FROM credentials WHERE id = ?`, id)
	var c rsql.Credential
	var kind, created string
	var expires sql.NullString
	if err := row.Scan(&c.ID, &c.UserID, &kind, &c.PublicKeyOrHash, &c.Label, &created, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	c.Kind = rsql.CredentialKind(kind)
	if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
		c.CreatedAt = t
	}
	if expires.Valid {
		if t, err := time.Parse(time.RFC3339Nano, expires.String); err == nil {
			c.ExpiresAt = &t
		}
	}
	return &c, nil
}

func (s *credentialStore) ListByUser(ctx context.Context, userID string) ([]*rsql.Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+credentialColumns+` FROM credentials WHERE user_id = ? ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*rsql.Credential
	for rows.Next() {
		var c rsql.Credential
		var kind, created string
		var expires sql.NullString
		if err := rows.Scan(&c.ID, &c.UserID, &kind, &c.PublicKeyOrHash, &c.Label, &created, &expires); err != nil {
			return nil, err
		}
		c.Kind = rsql.CredentialKind(kind)
		if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
			c.CreatedAt = t
		}
		if expires.Valid {
			if t, err := time.Parse(time.RFC3339Nano, expires.String); err == nil {
				c.ExpiresAt = &t
			}
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (s *credentialStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE id = ?`, id)
	return err
}

// Compile-time assertions that our impls satisfy the contracts.
var (
	_ rsql.BrainStore      = (*brainStore)(nil)
	_ rsql.MembershipStore = (*membershipStore)(nil)
	_ rsql.CredentialStore = (*credentialStore)(nil)
)
