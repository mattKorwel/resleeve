package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/mattkorwel/resleeve/internal/event"
)

// Ingester is the daemon-side interface used by reconcile to commit
// events. The local daemon's *agent.Daemon satisfies it via IngestBatch.
type Ingester interface {
	IngestBatch(ctx context.Context, sessionID string, events []event.Event) error
}

// ReconcileOnce opens opencode.db READ-ONLY and re-ingests every session's
// messages + parts as normalized events. Deterministic UUIDs keyed on
// opencode's ses_/msg_/prt_ ids + INSERT OR IGNORE on
// (session_id, event_uuid) make this safe to run repeatedly and safe
// against events the live SSE path already captured.
//
// It is a no-op (returns nil) when opencode is not the SQLite era — there
// is nothing to read and no legacy reader by design. Per-session events
// are flushed as one batch each.
func (a *Adapter) ReconcileOnce(ctx context.Context, ing Ingester) error {
	_, dbPath, err := resolveDBPath()
	if err != nil {
		return nil // no home dir / data dir; nothing to reconcile
	}
	if !sqliteEraPresent(dbPath) {
		// Pre-SQLite (or db not created yet): refuse silently rather than
		// falling back to a legacy reader. Detect surfaces the upgrade
		// message to the operator.
		return nil
	}

	db, err := openReadOnly(dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("opencode reconcile: %w", err)
	}
	defer db.Close()

	sessions, err := readSessions(ctx, db)
	if err != nil {
		return fmt.Errorf("opencode reconcile: %w", err)
	}

	for _, s := range sessions {
		evs, err := eventsForSession(ctx, db, s)
		if err != nil {
			return fmt.Errorf("opencode reconcile session %s: %w", s.ID, err)
		}
		if len(evs) == 0 {
			continue
		}
		if err := ing.IngestBatch(ctx, s.ID, evs); err != nil {
			return fmt.Errorf("opencode reconcile ingest %s: %w", s.ID, err)
		}
	}
	return nil
}
