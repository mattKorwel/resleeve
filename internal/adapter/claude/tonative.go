package claude

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// ToNative renders a captured event stream back into Claude Code's
// native on-disk format.
//
// Replay mode: emit one JSONL line per event, preferring
// event.Vendor.NativePayload (verbatim CC record bytes) when present.
// Events without a native payload — most commonly the session_start
// events resleeve synthesizes in the reconcile sweep — are dropped
// from the output, since CC's --resume reconstructs the session-start
// state from the filename + first real record. Events are emitted
// sorted by (seq, event_uuid) per the ordering spec in
// round-2/04-event-schema.md (amended in F4).
//
// Prime mode: synthesize a markdown opening prompt summarizing the
// captured activity. Lossy by design; see
// docs/design/round-4/01-conversation-transport.md. Callers that hand
// us a SessionView via Hydrate get the full Context block populated;
// direct ToNative callers (no session view) get a placeholder Context
// block sourced from the events themselves.
func (a *Adapter) ToNative(ctx context.Context, events []event.Event, mode adapter.RenderMode) ([]byte, error) {
	if mode == adapter.RenderModeAuto {
		mode = adapter.RenderModeReplay
	}
	switch mode {
	case adapter.RenderModeReplay:
		return a.toNativeReplay(events)
	case adapter.RenderModePrime:
		return a.toNativePrime(sessionViewFromEvents(events), events)
	default:
		return nil, fmt.Errorf("claude.ToNative: unknown mode %q", mode)
	}
}

// sessionViewFromEvents constructs a minimal SessionView from the
// events themselves, for ToNative-direct callers that don't have one.
// It pulls session_id from the first event with one; everything else
// stays zero so the prime renderer falls back to "(unknown)".
func sessionViewFromEvents(events []event.Event) adapter.SessionView {
	var view adapter.SessionView
	for _, e := range events {
		if e.SessionID != "" {
			view.SessionID = e.SessionID
			break
		}
	}
	view.CLI = Name
	return view
}

func (a *Adapter) toNativeReplay(events []event.Event) ([]byte, error) {
	sorted := make([]event.Event, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Seq != sorted[j].Seq {
			return sorted[i].Seq < sorted[j].Seq
		}
		return sorted[i].EventUUID < sorted[j].EventUUID
	})

	var buf bytes.Buffer
	for _, e := range sorted {
		payload := e.Vendor.NativePayload
		if len(payload) == 0 {
			continue
		}
		buf.Write(payload)
		// Ensure exactly one trailing newline.
		if len(payload) == 0 || payload[len(payload)-1] != '\n' {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes(), nil
}
