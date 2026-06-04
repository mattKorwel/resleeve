package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// IngestBatch is the central capture entry point. Given a session id
// and a batch of events, it:
//   - auto-creates the session if absent (using metadata from the first event)
//   - appends events idempotently (INSERT OR IGNORE on (session_id, event_uuid))
//   - heartbeats the slot tied to the events
//
// Idempotency on retries is guaranteed by:
//   - INSERT OR IGNORE in the storage layer (Stage 2)
//   - deterministic event UUIDs from each adapter (Stage 3b+)
func (d *Daemon) IngestBatch(ctx context.Context, sessionID string, events []event.Event) error {
	if sessionID == "" {
		return errors.New("ingest: empty session id")
	}
	if len(events) == 0 {
		return nil
	}

	// Create-if-absent in one atomic step. The storage layer's Create
	// is INSERT OR IGNORE, so two near-simultaneous first-event hooks
	// for the same session can both call here without racing into a
	// PK-violation error (and silently dropping events).
	ses, err := d.createSessionFromFirstEvent(ctx, sessionID, events[0])
	if err != nil {
		return fmt.Errorf("ingest: create session: %w", err)
	}

	if err := d.store.Events().Append(ctx, events); err != nil {
		return fmt.Errorf("ingest: append events: %w", err)
	}

	// Recompute sessions.event_count from the events table. Recompute
	// (rather than +len(events)) is intentional: Append is idempotent
	// via INSERT OR IGNORE, so retrying the same batch must not
	// double-count.
	if err := d.store.Sessions().SyncEventCount(ctx, sessionID); err != nil {
		return fmt.Errorf("ingest: sync event_count: %w", err)
	}

	// Heartbeat the slot from the first event. All events in a batch
	// should share the same slot in practice; if not, the first event's
	// slot wins for the heartbeat. Slots are machine-local — never
	// synced upstream.
	if err := d.store.Slots().Heartbeat(ctx, events[0].Slot, sessionID); err != nil {
		return fmt.Errorf("ingest: heartbeat: %w", err)
	}

	// Push-on-commit: enqueue the session row + each event to the local
	// outbox so the SyncClient's drain goroutine ships them to upstream.
	// Outbox enqueue is idempotent on (kind, key) so re-ingest of the
	// same batch (e.g. from sync pull on a peer device) doesn't duplicate.
	if d.sync != nil {
		if err := d.sync.EnqueueSession(ctx, ses); err != nil {
			return fmt.Errorf("ingest: enqueue session: %w", err)
		}
		if err := d.sync.EnqueueEvents(ctx, events); err != nil {
			return fmt.Errorf("ingest: enqueue events: %w", err)
		}
	}
	return nil
}

func (d *Daemon) createSessionFromFirstEvent(ctx context.Context, sessionID string, first event.Event) (*rsql.Session, error) {
	cwd := ""
	if first.Kind == event.KindSessionStart && len(first.Content) > 0 {
		// Try to pull cwd from session_start content.
		var c struct {
			Cwd string `json:"cwd"`
		}
		_ = json.Unmarshal(first.Content, &c)
		cwd = c.Cwd
	}
	startedAt := first.Timestamp
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	ses := &rsql.Session{
		ID:         sessionID,
		Slot:       first.Slot,
		CLI:        first.Vendor.Name,
		CLIVersion: first.Vendor.Version,
		Cwd:        cwd,
		StartedAt:  startedAt,
		Status:     rsql.SessionStatusActive,
	}
	if err := d.store.Sessions().Create(ctx, ses); err != nil {
		return nil, err
	}
	return ses, nil
}
