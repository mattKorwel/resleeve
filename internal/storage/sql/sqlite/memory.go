package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/memory"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

type memoryStore struct{ db *sql.DB }

// --- scopes ---

func (s *memoryStore) CreateScope(ctx context.Context, sc *memory.Scope) error {
	if sc.Path == "" {
		return memory.ErrEmptyPath
	}
	if !sc.Kind.Valid() {
		return memory.ErrInvalidKind
	}
	now := time.Now().UTC()
	if sc.CreatedAt.IsZero() {
		sc.CreatedAt = now
	}
	sc.UpdatedAt = now
	doNotInherit := 0
	if sc.DoNotInherit {
		doNotInherit = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO scopes
		(path, kind, title, description, cwd, do_not_inherit, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		sc.Path, string(sc.Kind), sc.Title, sc.Description, sc.Cwd, doNotInherit,
		sc.CreatedAt.Format(time.RFC3339Nano), sc.UpdatedAt.Format(time.RFC3339Nano),
	)
	return err
}

func (s *memoryStore) GetScope(ctx context.Context, path string) (*memory.Scope, error) {
	row := s.db.QueryRowContext(ctx, `SELECT path, kind, title, description, cwd, do_not_inherit, created_at, updated_at
		FROM scopes WHERE path = ?`, path)
	return scanScope(row)
}

// UpdateScope upserts a scope. Caller controls CreatedAt on first insert
// (or it's set to now); UpdatedAt is always touched.
func (s *memoryStore) UpdateScope(ctx context.Context, sc *memory.Scope) error {
	if sc.Path == "" {
		return memory.ErrEmptyPath
	}
	if !sc.Kind.Valid() {
		return memory.ErrInvalidKind
	}
	now := time.Now().UTC()
	if sc.CreatedAt.IsZero() {
		sc.CreatedAt = now
	}
	sc.UpdatedAt = now
	doNotInherit := 0
	if sc.DoNotInherit {
		doNotInherit = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO scopes
		(path, kind, title, description, cwd, do_not_inherit, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			kind = excluded.kind,
			title = excluded.title,
			description = excluded.description,
			cwd = excluded.cwd,
			do_not_inherit = excluded.do_not_inherit,
			updated_at = excluded.updated_at`,
		sc.Path, string(sc.Kind), sc.Title, sc.Description, sc.Cwd, doNotInherit,
		sc.CreatedAt.Format(time.RFC3339Nano), sc.UpdatedAt.Format(time.RFC3339Nano),
	)
	return err
}

// DeleteScope refuses if any child scopes exist (per Q3). Cascade
// cleans up plans/learnings on the target scope itself.
func (s *memoryStore) DeleteScope(ctx context.Context, path string) error {
	if path == "" {
		return memory.ErrEmptyPath
	}
	pattern := likeEscape(path+"/") + "%"
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scopes WHERE path LIKE ? ESCAPE '\'`, pattern,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return memory.ErrScopeHasChildren
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM scopes WHERE path = ?`, path)
	return err
}

func (s *memoryStore) ListScopes(ctx context.Context) ([]*memory.Scope, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, kind, title, description, cwd, do_not_inherit, created_at, updated_at
		FROM scopes ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*memory.Scope
	for rows.Next() {
		sc, err := scanScope(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanScope(row scanner) (*memory.Scope, error) {
	var sc memory.Scope
	var kind string
	var doNotInherit int
	var createdAt, updatedAt string
	if err := row.Scan(&sc.Path, &kind, &sc.Title, &sc.Description, &sc.Cwd, &doNotInherit, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	sc.Kind = memory.ScopeKind(kind)
	sc.DoNotInherit = doNotInherit != 0
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		sc.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
		sc.UpdatedAt = t
	}
	return &sc, nil
}

// --- plans (append-only versions; HEAD = max version) ---

// AppendPlanVersion appends an immutable plan version with optimistic
// concurrency. See the MemoryStore interface doc for the base-version /
// force contract. The HEAD read + the insert run inside a single
// transaction so two concurrent appends can't both observe the same HEAD
// and write the same version number (the PK on (scope, name, version)
// is the backstop, the tx is the correctness primitive).
func (s *memoryStore) AppendPlanVersion(ctx context.Context, scope, name, content, author string, baseVersion int64, force bool) (*memory.Plan, error) {
	if scope == "" {
		return nil, memory.ErrEmptyPath
	}
	if name == "" {
		name = memory.DefaultPlanSlot
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	head, err := scanPlan(tx.QueryRowContext(ctx,
		`SELECT scope, name, version, content, author_user_id, parent_version, created_at
			FROM plan_versions WHERE scope = ? AND name = ?
			ORDER BY version DESC LIMIT 1`, scope, name))
	if err != nil && !errors.Is(err, rsql.ErrNotFound) {
		return nil, err
	}
	headVersion := int64(0)
	if head != nil {
		headVersion = head.Version
	}

	if !force && baseVersion != headVersion {
		// Stale (or expected-new against an existing plan): surface the
		// current HEAD so the caller can reconcile.
		return nil, &memory.PlanConflictError{Scope: scope, Name: name, Head: head}
	}

	next := headVersion + 1
	now := time.Now().UTC()
	parent := headVersion // parent of the appended row is the version it built on
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO plan_versions
			(scope, name, version, content, author_user_id, parent_version, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
		scope, name, next, content, author, parent, now.Format(time.RFC3339Nano),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &memory.Plan{
		Scope:         scope,
		Name:          name,
		Version:       next,
		Content:       content,
		Author:        author,
		ParentVersion: parent,
		UpdatedAt:     now,
	}, nil
}

func (s *memoryStore) GetPlan(ctx context.Context, scope, slot string) (*memory.Plan, error) {
	if slot == "" {
		slot = memory.DefaultPlanSlot
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT scope, name, version, content, author_user_id, parent_version, created_at
			FROM plan_versions WHERE scope = ? AND name = ?
			ORDER BY version DESC LIMIT 1`,
		scope, slot)
	return scanPlan(row)
}

func (s *memoryStore) GetPlanVersion(ctx context.Context, scope, slot string, version int64) (*memory.Plan, error) {
	if slot == "" {
		slot = memory.DefaultPlanSlot
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT scope, name, version, content, author_user_id, parent_version, created_at
			FROM plan_versions WHERE scope = ? AND name = ? AND version = ?`,
		scope, slot, version)
	return scanPlan(row)
}

// ListPlans returns the HEAD of every slot at scope (one row per name).
func (s *memoryStore) ListPlans(ctx context.Context, scope string) ([]*memory.Plan, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pv.scope, pv.name, pv.version, pv.content, pv.author_user_id, pv.parent_version, pv.created_at
			FROM plan_versions pv
			JOIN (SELECT name, MAX(version) AS v FROM plan_versions WHERE scope = ? GROUP BY name) h
			  ON pv.name = h.name AND pv.version = h.v
			WHERE pv.scope = ?
			ORDER BY pv.name`,
		scope, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*memory.Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPlanVersions returns the full version history of one slot, oldest first.
func (s *memoryStore) ListPlanVersions(ctx context.Context, scope, slot string) ([]*memory.Plan, error) {
	if slot == "" {
		slot = memory.DefaultPlanSlot
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope, name, version, content, author_user_id, parent_version, created_at
			FROM plan_versions WHERE scope = ? AND name = ? ORDER BY version ASC`,
		scope, slot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*memory.Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *memoryStore) DeletePlan(ctx context.Context, scope, slot string) error {
	if slot == "" {
		slot = memory.DefaultPlanSlot
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM plan_versions WHERE scope = ? AND name = ?`, scope, slot)
	return err
}

func scanPlan(row scanner) (*memory.Plan, error) {
	var p memory.Plan
	var createdAt string
	if err := row.Scan(&p.Scope, &p.Name, &p.Version, &p.Content, &p.Author, &p.ParentVersion, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		p.UpdatedAt = t
	}
	return &p, nil
}

// --- learnings ---

func (s *memoryStore) AppendLearning(ctx context.Context, l *memory.Learning) error {
	if l.Scope == "" {
		return memory.ErrEmptyPath
	}
	if l.ID == "" {
		return errors.New("memory: empty learning ID")
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now().UTC()
	}
	var supersedes sql.NullString
	if l.SupersedesID != nil {
		supersedes = sql.NullString{Valid: true, String: *l.SupersedesID}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO learnings (id, scope, content, supersedes_id, author_user_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		l.ID, l.Scope, l.Content, supersedes, l.Author, l.CreatedAt.Format(time.RFC3339Nano),
	)
	return err
}

func (s *memoryStore) GetLearning(ctx context.Context, id string) (*memory.Learning, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, scope, content, supersedes_id, author_user_id, created_at FROM learnings WHERE id = ?`, id)
	return scanLearning(row)
}

func (s *memoryStore) ListLearnings(ctx context.Context, scope string, includeSuperseded bool) ([]*memory.Learning, error) {
	var q string
	if includeSuperseded {
		q = `SELECT id, scope, content, supersedes_id, author_user_id, created_at
			FROM learnings WHERE scope = ?
			ORDER BY created_at ASC`
	} else {
		// Hide rows that have been superseded by another learning. A learning
		// is "superseded" when some other row has supersedes_id = this.id.
		q = `SELECT l1.id, l1.scope, l1.content, l1.supersedes_id, l1.author_user_id, l1.created_at
			FROM learnings l1
			WHERE l1.scope = ?
			  AND NOT EXISTS (
			      SELECT 1 FROM learnings l2 WHERE l2.supersedes_id = l1.id
			  )
			ORDER BY l1.created_at ASC`
	}
	rows, err := s.db.QueryContext(ctx, q, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*memory.Learning
	for rows.Next() {
		l, err := scanLearning(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func scanLearning(row scanner) (*memory.Learning, error) {
	var l memory.Learning
	var supersedes sql.NullString
	var createdAt string
	if err := row.Scan(&l.ID, &l.Scope, &l.Content, &supersedes, &l.Author, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	if supersedes.Valid {
		v := supersedes.String
		l.SupersedesID = &v
	}
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		l.CreatedAt = t
	}
	return &l, nil
}

// likeEscape escapes %, _, and \ in a LIKE pattern when used with
// `ESCAPE '\'`.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
