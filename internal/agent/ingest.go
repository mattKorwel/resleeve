package agent

import (
	"context"
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

	if _, err := d.store.Sessions().Get(ctx, sessionID); err != nil {
		if !errors.Is(err, rsql.ErrNotFound) {
			return fmt.Errorf("ingest: get session: %w", err)
		}
		if err := d.createSessionFromFirstEvent(ctx, sessionID, events[0]); err != nil {
			return fmt.Errorf("ingest: create session: %w", err)
		}
	}

	if err := d.store.Events().Append(ctx, events); err != nil {
		return fmt.Errorf("ingest: append events: %w", err)
	}

	// Heartbeat the slot from the first event. All events in a batch
	// should share the same slot in practice; if not, the first event's
	// slot wins for the heartbeat.
	if err := d.store.Slots().Heartbeat(ctx, events[0].Slot, sessionID); err != nil {
		return fmt.Errorf("ingest: heartbeat: %w", err)
	}
	return nil
}

func (d *Daemon) createSessionFromFirstEvent(ctx context.Context, sessionID string, first event.Event) error {
	cwd := ""
	if first.Kind == event.KindSessionStart && len(first.Content) > 0 {
		// Try to pull cwd from session_start content.
		var c struct {
			Cwd string `json:"cwd"`
		}
		_ = decodeIgnoringUnknown(first.Content, &c)
		cwd = c.Cwd
	}
	ses := &rsql.Session{
		ID:         sessionID,
		Slot:       first.Slot,
		CLI:        first.Vendor.Name,
		CLIVersion: first.Vendor.Version,
		Cwd:        cwd,
		StartedAt:  time.Now().UTC(),
		Status:     rsql.SessionStatusActive,
	}
	return d.store.Sessions().Create(ctx, ses)
}
