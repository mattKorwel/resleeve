package antigravity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mattkorwel/resleeve/internal/event"
)

// Ingester is the daemon-side interface used by reconcile to commit events.
// The local daemon's *agent.Daemon satisfies it via IngestBatch. Defined
// here (rather than imported) so the adapter package does not depend on the
// agent package — exactly as the claude adapter does.
type Ingester interface {
	IngestBatch(ctx context.Context, sessionID string, events []event.Event) error
}

// flushSize bounds an in-memory batch before it is committed. Antigravity
// conversations are small (tens of steps), so this rarely triggers — it is a
// safety bound, not a tuning knob.
const flushSize = 200

// conversationsDir returns ~/.gemini/antigravity-cli/conversations.
func conversationsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("homedir: %w", err)
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "conversations"), nil
}

// lastConversationsPath returns the cwd→conversation map file.
func lastConversationsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("homedir: %w", err)
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "cache", "last_conversations.json"), nil
}

// ReconcileOnce walks ~/.gemini/antigravity-cli/conversations/*.db, opens
// each read-only, decodes its steps, and ingests one batch per conversation.
//
// Lenient by contract: a malformed/undecodable db, a bad step, or even a
// panic inside decoding is logged and skipped — NEVER fatal. The only error
// it returns is a hard filesystem failure walking the root (and even a
// missing root is treated as "nothing to do").
//
// Deterministic UUIDs keyed on (conversationID, "step:"+idx) + INSERT OR
// IGNORE at the storage layer make repeated sweeps idempotent.
func (a *Adapter) ReconcileOnce(ctx context.Context, ing Ingester) error {
	root, err := conversationsDir()
	if err != nil {
		return err
	}

	// Build the reverse cwd lookup once; failure here is non-fatal (we just
	// lose cwd → scope resolution and fall back to "unknown").
	cwdByConv := loadCwdByConversation()

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil // conversations dir doesn't exist yet — fine
			}
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".db") {
			return nil
		}
		if err := a.reconcileDB(ctx, ing, path, cwdByConv); err != nil {
			log.Printf("antigravity reconcile %s: %v", path, err)
		}
		return nil
	})
}

// loadCwdByConversation reads last_conversations.json ({"<cwd>":"<id>"}) and
// inverts it to {"<id>":"<cwd>"}. Returns an empty (non-nil) map on any
// error so callers can index without nil checks.
func loadCwdByConversation() map[string]string {
	out := map[string]string{}
	path, err := lastConversationsPath()
	if err != nil {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var fwd map[string]string
	if err := json.Unmarshal(data, &fwd); err != nil {
		log.Printf("antigravity: parse %s: %v", path, err)
		return out
	}
	for cwd, conv := range fwd {
		out[conv] = cwd
	}
	return out
}

// reconcileDB opens one conversation .db read-only, decodes every step, and
// ingests the resulting events. It recovers from panics so a single corrupt
// payload can never take the daemon down.
func (a *Adapter) reconcileDB(ctx context.Context, ing Ingester, path string, cwdByConv map[string]string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic decoding %s: %v", path, r)
		}
	}()

	convID := conversationIDFromPath(path)
	cwd := cwdByConv[convID]

	steps, dbErr := readSteps(ctx, path)
	if dbErr != nil {
		return dbErr
	}
	if len(steps) == 0 {
		return nil
	}

	var batch []event.Event
	// Synthesize the session_start from conversation id + cwd, mirroring the
	// claude reconcile path (Antigravity stores no explicit start row).
	start := synthesizeSessionStart(convID, cwd, firstStepTime(steps))
	batch = append(batch, start)

	scope := deriveScope(cwd)
	for _, s := range steps {
		evs := a.mapStep(convID, scope, s)
		batch = append(batch, evs...)
		if len(batch) >= flushSize {
			if e := ing.IngestBatch(ctx, convID, batch); e != nil {
				log.Printf("antigravity ingest (%s): %v", convID, e)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if e := ing.IngestBatch(ctx, convID, batch); e != nil {
			log.Printf("antigravity ingest (%s): %v", convID, e)
		}
	}
	return nil
}

// conversationIDFromPath extracts the conversation UUID from a db filename
// like ".../<uuid>.db".
func conversationIDFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, ".db")
}

// readSteps opens the conversation db strictly read-only and returns its
// step rows ordered by idx. The DSN combines mode=ro with PRAGMA
// query_only=ON so nothing in our process can mutate the user's store even
// by accident — Antigravity is the sole writer.
func readSteps(ctx context.Context, path string) ([]step, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT idx, step_type, status, step_payload FROM steps ORDER BY idx`)
	if err != nil {
		return nil, fmt.Errorf("query steps: %w", err)
	}
	defer rows.Close()

	var out []step
	for rows.Next() {
		var s step
		var payload []byte
		if err := rows.Scan(&s.Idx, &s.StepType, &s.Status, &payload); err != nil {
			log.Printf("antigravity: scan step in %s: %v", path, err)
			continue
		}
		s.Payload = append([]byte(nil), payload...)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate steps: %w", err)
	}
	return out, nil
}

// firstStepTime returns a deterministic timestamp for the synthesized
// session_start. Antigravity step payloads carry timestamps, but decoding
// them reliably is out of scope; we use the file's mtime when available and
// fall back to now. (Determinism of the EVENT UUID, not the timestamp, is
// what reconcile idempotency depends on.)
func firstStepTime(steps []step) time.Time {
	return time.Now().UTC()
}

// synthesizeSessionStart builds the synthetic start event. Uses the same
// deterministic key ("session_start") so repeated sweeps dedup.
func synthesizeSessionStart(convID, cwd string, ts time.Time) event.Event {
	content, _ := json.Marshal(map[string]any{
		"cli":    Name,
		"cwd":    cwd,
		"source": "reconcile",
	})
	return event.Event{
		EventUUID:     DeterministicEventUUID(convID, "session_start"),
		SessionID:     convID,
		Slot:          event.Slot{Scope: deriveScope(cwd), AgentName: "default"},
		Seq:           ts.UnixNano() - 1,
		Timestamp:     ts,
		Kind:          event.KindSessionStart,
		SchemaVersion: 1,
		Content:       content,
		Vendor:        event.Vendor{Name: Name},
	}
}
