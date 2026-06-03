package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.up.sql
var migrationsFS embed.FS

// Store implements rsql.Store backed by SQLite.
type Store struct {
	db       *sql.DB
	sessions *sessionStore
	events   *eventStore
	slots    *slotStore
	users    *userStore
	memory   *memoryStore
}

// Open opens (and auto-migrates) a SQLite database at the given DSN.
// Recommended DSN: "file:./data.db?_pragma=journal_mode=WAL&_pragma=foreign_keys=on".
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	s := &Store{db: db}
	s.sessions = &sessionStore{db: db}
	s.events = &eventStore{db: db}
	s.slots = &slotStore{db: db}
	s.users = &userStore{db: db}
	s.memory = &memoryStore{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Sessions returns the SessionStore view.
func (s *Store) Sessions() rsql.SessionStore { return s.sessions }

// Events returns the EventStore view.
func (s *Store) Events() rsql.EventStore { return s.events }

// Slots returns the SlotStore view.
func (s *Store) Slots() rsql.SlotStore { return s.slots }

// Users returns the UserStore view.
func (s *Store) Users() rsql.UserStore { return s.users }

// Memory returns the MemoryStore view.
func (s *Store) Memory() rsql.MemoryStore { return s.memory }

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := versionOf(name)
		if version == 0 {
			return fmt.Errorf("migration %q has no leading version number", name)
		}
		var dummy int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&dummy)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %d: %w", version, err)
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("record migration %d: %w", version, err)
		}
	}
	return nil
}

func versionOf(name string) int {
	n := 0
	for _, r := range name {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// ---- sessionStore ----

type sessionStore struct{ db *sql.DB }

// Create inserts a session. Idempotent: a duplicate ID is a silent
// no-op via INSERT OR IGNORE, so two near-simultaneous first-events
// for the same session can both call Create without one of them
// racing into a PK-violation error. Matches the spirit of the recent
// ori "atomic sticky pointer" fix — same shape of bug, different
// fields. See docs/design/round-2/02-journey-01-decisions.md Q2.
func (s *sessionStore) Create(ctx context.Context, ses *rsql.Session) error {
	var endedAt sql.NullString
	if ses.EndedAt != nil {
		endedAt = sql.NullString{Valid: true, String: ses.EndedAt.UTC().Format(time.RFC3339Nano)}
	}
	var exitStatus sql.NullInt64
	if ses.ExitStatus != nil {
		exitStatus = sql.NullInt64{Valid: true, Int64: int64(*ses.ExitStatus)}
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO sessions
		(id, scope, agent_name, cli, cli_version, cwd, git_branch, model, started_at, ended_at, event_count, status, exit_status)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ses.ID, ses.Slot.Scope, ses.Slot.AgentName, ses.CLI, ses.CLIVersion, ses.Cwd, ses.GitBranch, ses.Model,
		ses.StartedAt.UTC().Format(time.RFC3339Nano), endedAt, ses.EventCount, string(ses.Status), exitStatus,
	)
	return err
}

func (s *sessionStore) Get(ctx context.Context, id string) (*rsql.Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, scope, agent_name, cli, cli_version, cwd, git_branch, model,
		started_at, ended_at, event_count, status, exit_status
		FROM sessions WHERE id = ?`, id)
	var ses rsql.Session
	var startedAt string
	var endedAt sql.NullString
	var exitStatus sql.NullInt64
	var status string
	if err := row.Scan(
		&ses.ID, &ses.Slot.Scope, &ses.Slot.AgentName,
		&ses.CLI, &ses.CLIVersion, &ses.Cwd, &ses.GitBranch, &ses.Model,
		&startedAt, &endedAt, &ses.EventCount, &status, &exitStatus,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, startedAt); err == nil {
		ses.StartedAt = t
	}
	if endedAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, endedAt.String); err == nil {
			ses.EndedAt = &t
		}
	}
	if exitStatus.Valid {
		v := int(exitStatus.Int64)
		ses.ExitStatus = &v
	}
	ses.Status = rsql.SessionStatus(status)
	return &ses, nil
}

// List returns sessions matching f, ordered by started_at DESC.
func (s *sessionStore) List(ctx context.Context, f rsql.SessionFilter) ([]*rsql.Session, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	q := `SELECT id, scope, agent_name, cli, cli_version, cwd, git_branch, model,
		started_at, ended_at, event_count, status, exit_status
		FROM sessions WHERE 1=1`
	var args []any
	if f.Scope != "" {
		q += ` AND scope = ?`
		args = append(args, f.Scope)
	}
	if f.AgentName != "" {
		q += ` AND agent_name = ?`
		args = append(args, f.AgentName)
	}
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, string(f.Status))
	}
	if f.Since != nil {
		q += ` AND started_at >= ?`
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*rsql.Session
	for rows.Next() {
		ses := &rsql.Session{}
		var startedAt string
		var endedAt sql.NullString
		var exitStatus sql.NullInt64
		var status string
		if err := rows.Scan(
			&ses.ID, &ses.Slot.Scope, &ses.Slot.AgentName,
			&ses.CLI, &ses.CLIVersion, &ses.Cwd, &ses.GitBranch, &ses.Model,
			&startedAt, &endedAt, &ses.EventCount, &status, &exitStatus,
		); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, startedAt); err == nil {
			ses.StartedAt = t
		}
		if endedAt.Valid {
			if t, err := time.Parse(time.RFC3339Nano, endedAt.String); err == nil {
				ses.EndedAt = &t
			}
		}
		if exitStatus.Valid {
			v := int(exitStatus.Int64)
			ses.ExitStatus = &v
		}
		ses.Status = rsql.SessionStatus(status)
		out = append(out, ses)
	}
	return out, rows.Err()
}

// ---- eventStore ----

type eventStore struct{ db *sql.DB }

func (s *eventStore) Append(ctx context.Context, events []event.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO events
		(event_uuid, session_id, seq, turn_id, parent_event_uuid, ts, kind, schema_version,
		 content, vendor_name, vendor_version, vendor_native_payload)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range events {
		var turnID, parentUUID sql.NullString
		if e.TurnID != nil {
			turnID = sql.NullString{Valid: true, String: *e.TurnID}
		}
		if e.ParentEventUUID != nil {
			parentUUID = sql.NullString{Valid: true, String: *e.ParentEventUUID}
		}
		content := e.Content
		if len(content) == 0 {
			content = json.RawMessage("{}")
		}
		var native interface{}
		if len(e.Vendor.NativePayload) > 0 {
			native = []byte(e.Vendor.NativePayload)
		}
		if _, err := stmt.ExecContext(ctx,
			e.EventUUID, e.SessionID, e.Seq, turnID, parentUUID,
			e.Timestamp.UTC().Format(time.RFC3339Nano), string(e.Kind), e.SchemaVersion,
			string(content), e.Vendor.Name, e.Vendor.Version, native,
		); err != nil {
			return fmt.Errorf("insert event %s: %w", e.EventUUID, err)
		}
	}
	return tx.Commit()
}

func (s *eventStore) List(ctx context.Context, sessionID string, sinceSeq int64, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_uuid, seq, turn_id, parent_event_uuid, ts, kind, schema_version,
		content, vendor_name, vendor_version, vendor_native_payload
		FROM events WHERE session_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		sessionID, sinceSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []event.Event
	for rows.Next() {
		e := event.Event{SessionID: sessionID}
		var turnID, parentUUID sql.NullString
		var ts, kind string
		var content, native []byte
		if err := rows.Scan(
			&e.EventUUID, &e.Seq, &turnID, &parentUUID,
			&ts, &kind, &e.SchemaVersion,
			&content, &e.Vendor.Name, &e.Vendor.Version, &native,
		); err != nil {
			return nil, err
		}
		e.Kind = event.Kind(kind)
		if turnID.Valid {
			v := turnID.String
			e.TurnID = &v
		}
		if parentUUID.Valid {
			v := parentUUID.String
			e.ParentEventUUID = &v
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.Timestamp = t
		}
		if len(content) > 0 {
			e.Content = json.RawMessage(content)
		}
		if len(native) > 0 {
			e.Vendor.NativePayload = json.RawMessage(native)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Search performs a portable substring search over events.content
// (JSON-as-TEXT). v1 uses LIKE; SQLite FTS5 + Postgres tsvector are
// pluggable upgrades behind the same Search contract.
func (s *eventStore) Search(ctx context.Context, query string, limit int) ([]rsql.EventSearchHit, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if query == "" {
		return nil, nil
	}
	// Escape LIKE wildcards in the user query.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	pattern := "%" + escaped + "%"

	rows, err := s.db.QueryContext(ctx, `SELECT event_uuid, session_id, seq, turn_id, parent_event_uuid,
		ts, kind, schema_version, content, vendor_name, vendor_version, vendor_native_payload
		FROM events WHERE content LIKE ? ESCAPE '\' ORDER BY ts DESC LIMIT ?`,
		pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []rsql.EventSearchHit
	for rows.Next() {
		var e event.Event
		var turnID, parentUUID sql.NullString
		var ts, kind string
		var content, native []byte
		if err := rows.Scan(
			&e.EventUUID, &e.SessionID, &e.Seq, &turnID, &parentUUID,
			&ts, &kind, &e.SchemaVersion,
			&content, &e.Vendor.Name, &e.Vendor.Version, &native,
		); err != nil {
			return nil, err
		}
		e.Kind = event.Kind(kind)
		if turnID.Valid {
			v := turnID.String
			e.TurnID = &v
		}
		if parentUUID.Valid {
			v := parentUUID.String
			e.ParentEventUUID = &v
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.Timestamp = t
		}
		if len(content) > 0 {
			e.Content = json.RawMessage(content)
		}
		if len(native) > 0 {
			e.Vendor.NativePayload = json.RawMessage(native)
		}
		out = append(out, rsql.EventSearchHit{
			Event:   e,
			Snippet: snippet(string(content), query, 120),
		})
	}
	return out, rows.Err()
}

// snippet returns up to width chars around the first case-insensitive
// match of needle in haystack. Best-effort, not algorithmically optimal.
func snippet(haystack, needle string, width int) string {
	if needle == "" {
		return ""
	}
	lh := strings.ToLower(haystack)
	ln := strings.ToLower(needle)
	idx := strings.Index(lh, ln)
	if idx < 0 {
		if len(haystack) > width {
			return haystack[:width] + "…"
		}
		return haystack
	}
	start := idx - width/2
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(haystack) {
		end = len(haystack)
	}
	prefix := ""
	if start > 0 {
		prefix = "…"
	}
	suffix := ""
	if end < len(haystack) {
		suffix = "…"
	}
	return prefix + haystack[start:end] + suffix
}

// ---- slotStore ----

type slotStore struct{ db *sql.DB }

func (s *slotStore) Heartbeat(ctx context.Context, slot event.Slot, currentSessionID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var current sql.NullString
	if currentSessionID != "" {
		current = sql.NullString{Valid: true, String: currentSessionID}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO slots (scope, agent_name, last_heartbeat_at, current_session_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(scope, agent_name) DO UPDATE
		SET last_heartbeat_at = excluded.last_heartbeat_at,
		    current_session_id = excluded.current_session_id`,
		slot.Scope, slot.AgentName, now, current)
	return err
}

func (s *slotStore) Get(ctx context.Context, slot event.Slot) (*rsql.SlotState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT last_heartbeat_at, current_session_id
		FROM slots WHERE scope = ? AND agent_name = ?`, slot.Scope, slot.AgentName)
	var last string
	var current sql.NullString
	if err := row.Scan(&last, &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, rsql.ErrNotFound
		}
		return nil, err
	}
	state := &rsql.SlotState{Slot: slot}
	if t, err := time.Parse(time.RFC3339Nano, last); err == nil {
		state.LastHeartbeatAt = t
	}
	if current.Valid {
		state.CurrentSessionID = current.String
	}
	return state, nil
}
