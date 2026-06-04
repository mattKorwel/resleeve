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
// Replay mode (the only mode this commit implements): emit one JSONL
// line per event, preferring event.Vendor.NativePayload (verbatim CC
// record bytes) when present. Events without a native payload — most
// commonly the session_start events resleeve synthesizes in the
// reconcile sweep — are dropped from the output, since CC's --resume
// reconstructs the session-start state from the filename + first real
// record. Events are emitted sorted by (seq, event_uuid) per the
// ordering spec in round-2/04-event-schema.md (amended in F4).
//
// Prime mode is round-4 scope but lands in a follow-up commit.
func (a *Adapter) ToNative(ctx context.Context, events []event.Event, mode adapter.RenderMode) ([]byte, error) {
	if mode == adapter.RenderModeAuto {
		mode = adapter.RenderModeReplay
	}
	switch mode {
	case adapter.RenderModeReplay:
		return a.toNativeReplay(events)
	case adapter.RenderModePrime:
		return nil, fmt.Errorf("%w: claude.ToNative prime mode", adapter.ErrNotImplemented)
	default:
		return nil, fmt.Errorf("claude.ToNative: unknown mode %q", mode)
	}
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
