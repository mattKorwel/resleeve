package antigravity

import (
	"context"
	"fmt"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/common"
	"github.com/mattkorwel/resleeve/internal/event"
)

// ToNative renders a captured event stream into the target's native form.
//
// Antigravity supports PRIME mode ONLY: it synthesizes a markdown opening
// prompt summarizing prior activity (via common.SynthesizePrime). REPLAY —
// re-writing a native conversation .db so `agy` resumes it byte-for-byte — is
// NOT supported: the on-disk format is an undocumented protobuf-in-SQLite
// store we can read best-effort but must not forge. Same-CLI resume of an
// antigravity-origin session is instead done natively via
// `agy --conversation <id>` (see NativeResumeCmd); cross-CLI / replay-in is
// prime/content-seed only.
func (a *Adapter) ToNative(ctx context.Context, events []event.Event, mode adapter.RenderMode) ([]byte, error) {
	if mode == adapter.RenderModeAuto {
		// Auto resolves to prime for antigravity: replay is unsupported, so
		// the only render we can produce from events is the prime prompt.
		mode = adapter.RenderModePrime
	}
	switch mode {
	case adapter.RenderModePrime:
		return common.SynthesizePrime(sessionViewFromEvents(events), events, common.PrimeOpts{})
	case adapter.RenderModeReplay:
		return nil, fmt.Errorf("antigravity.ToNative: replay mode is unsupported (cannot forge the undocumented protobuf-in-SQLite store); resume an antigravity-origin session natively via `agy --conversation <id>`, or use prime mode")
	default:
		return nil, fmt.Errorf("antigravity.ToNative: unknown mode %q", mode)
	}
}

// sessionViewFromEvents builds a minimal SessionView for ToNative-direct
// callers that lack one, so the prime renderer still has a session id.
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
