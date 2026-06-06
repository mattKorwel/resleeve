package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/mattkorwel/resleeve/internal/event"
)

// openReadOnly opens opencode.db with a strictly read-only connection.
// The daemon must never write opencode's store, so we open with mode=ro
// plus the query_only(1) pragma (non-mutating guarantee). We deliberately
// do NOT set immutable=1: opencode may be actively writing the WAL-mode db,
// and immutable disables locking — which can surface stale or torn pages.
// mode=ro keeps SQLite's normal locking so we read a consistent snapshot.
//
// The path is carried as a `file://` URI so spaces / special chars survive.
// It must be cross-platform: SQLite's URI form needs forward slashes and a
// leading slash, so a Windows path `C:\x\db` becomes `file:///C:/x/db` (not
// `file:C:%5Cx%5Cdb`, which SQLite rejects as an "invalid uri authority").
func openReadOnly(dbPath string) (*sql.DB, error) {
	p := filepath.ToSlash(dbPath)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive letter → /C:/x/db, so authority stays empty
	}
	u := url.URL{Scheme: "file", Path: p}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Set("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open opencode.db: %w", err)
	}
	// One reader connection is plenty and avoids modernc spawning writers.
	db.SetMaxOpenConns(1)
	return db, nil
}

// readSessionEvents reads every session (and its messages + parts) from
// the db and returns them as ordered, normalized events. opencode ids are
// time-sortable, so a final sort on (Seq, EventUUID) yields stable order.
func readSessionEvents(ctx context.Context, db *sql.DB) ([]event.Event, error) {
	sessions, err := readSessions(ctx, db)
	if err != nil {
		return nil, err
	}

	var out []event.Event
	for _, s := range sessions {
		evs, err := eventsForSession(ctx, db, s)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
	}
	return out, nil
}

// readSessions selects the SessionTable columns resleeve maps.
func readSessions(ctx context.Context, db *sql.DB) ([]sessionRow, error) {
	const q = `
SELECT id, project_id, directory, title, version, model, time_created, time_updated
FROM session
ORDER BY time_created, id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var out []sessionRow
	for rows.Next() {
		var s sessionRow
		var model []byte
		var directory, title, version sql.NullString
		if err := rows.Scan(&s.ID, &s.ProjectID, &directory, &title, &version, &model, &s.TimeCreated, &s.TimeUpdated); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		s.Directory = directory.String
		s.Title = title.String
		s.Version = version.String
		if len(model) > 0 {
			s.Model = append([]byte(nil), model...)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// eventsForSession assembles the session_start event plus every message
// (with its parts) for one session, sorted by (Seq, EventUUID).
func eventsForSession(ctx context.Context, db *sql.DB, s sessionRow) ([]event.Event, error) {
	out := []event.Event{sessionStartEvent(s, nil)}

	messages, err := readMessages(ctx, db, s.ID)
	if err != nil {
		return nil, err
	}
	partsByMsg, err := readParts(ctx, db, s.ID)
	if err != nil {
		return nil, err
	}

	for _, m := range messages {
		out = append(out, messageEvents(m, partsByMsg[m.ID], s.Directory, s.Version)...)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].EventUUID < out[j].EventUUID
	})
	return out, nil
}

// readMessages selects MessageTable rows for a session, ordered by the
// table's (time_created, id) index.
func readMessages(ctx context.Context, db *sql.DB, sessionID string) ([]messageRow, error) {
	const q = `
SELECT id, session_id, time_created, data
FROM message
WHERE session_id = ?
ORDER BY time_created, id`
	rows, err := db.QueryContext(ctx, q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var out []messageRow
	for rows.Next() {
		var m messageRow
		var data []byte
		if err := rows.Scan(&m.ID, &m.SessionID, &m.TimeCreated, &data); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Data = append([]byte(nil), data...)
		out = append(out, m)
	}
	return out, rows.Err()
}

// readParts selects PartTable rows for a session, grouped by message_id
// and ordered by (message_id, id) to match insertion order.
func readParts(ctx context.Context, db *sql.DB, sessionID string) (map[string][]partRow, error) {
	const q = `
SELECT id, message_id, session_id, time_created, data
FROM part
WHERE session_id = ?
ORDER BY message_id, id`
	rows, err := db.QueryContext(ctx, q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query parts: %w", err)
	}
	defer rows.Close()

	out := map[string][]partRow{}
	for rows.Next() {
		var p partRow
		var data []byte
		if err := rows.Scan(&p.ID, &p.MessageID, &p.SessionID, &p.TimeCreated, &data); err != nil {
			return nil, fmt.Errorf("scan part: %w", err)
		}
		p.Data = append([]byte(nil), data...)
		out[p.MessageID] = append(out[p.MessageID], p)
	}
	return out, rows.Err()
}
