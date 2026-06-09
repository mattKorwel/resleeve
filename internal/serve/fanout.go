package serve

// Brain-keyed fan-out hub (round-13 slice 1).
//
// The sync model is notify→pull: a memory write to brain X must reach the
// *other members of X* proactively (otherwise a shared brain only updates
// on the slow periodic pull and feels dead). handlePush publishes the
// (already brain-prefixed, plaintext-as-pushed) memory row to this hub
// under its brain_id; handleSSE subscribes to its own acting brain_id and
// forwards the row down the live stream. The periodic pull remains the
// correctness backstop — SSE is purely the low-latency optimization and
// degrades gracefully if a notify is dropped.
//
// Before this slice the fan-out was a single global subscriber set:
// every push went to *every* SSE channel and each handler filtered by its
// own brain prefix. That delivered cross-member rows (the prefix matched)
// but amplified every brain's writes to every connection. Keying the hub
// on brain_id routes a push to brain X only to subscribers whose acting
// brain is X — no cross-brain amplification, no client-side dropping.
//
// SEAM: fanoutHub is the pub/sub seam. The in-process impl below is
// sufficient for a single serve instance (stage A on the scale ladder).
// For horizontal scale-out (stage B) SSE connections pin to one instance,
// so a write on instance A must reach subscribers on instance B — that
// needs a backplane. A Postgres LISTEN/NOTIFY impl of this same interface
// (Publish → NOTIFY brain_id; Subscribe → LISTEN + a local relay) is the
// deferred next slice; it slots in here with no handler changes. NATS /
// Redis sit behind the same seam later. DO NOT implement the backplane in
// this slice.

import "sync"

// fanoutHub is the brain-keyed pub/sub seam between handlePush (publisher)
// and handleSSE (subscriber). Implementations route a published row to
// exactly the subscribers of its brain.
//
// Contract:
//   - Subscribe(brainID) returns a buffered receive channel plus a cancel
//     func the caller MUST invoke (defer) to unregister and release the
//     channel. The channel is never closed by the hub.
//   - Publish(brainID, row) is non-blocking: a subscriber whose buffer is
//     full has the row dropped (the periodic pull / SSE reconnect backlog
//     is the catch-up path). Publish never blocks the push handler on a
//     slow SSE client.
//   - Both methods are safe for concurrent use.
type fanoutHub interface {
	Subscribe(brainID string) (<-chan PushRow, func())
	Publish(brainID string, row PushRow)
}

// inProcessHub is the single-instance fanoutHub: an in-memory map from
// brain_id to the set of subscriber channels for that brain. Sufficient
// for stage A (one serve process). Replaced by a Postgres LISTEN/NOTIFY
// backplane behind the same interface for scale-out (deferred).
type inProcessHub struct {
	mu   sync.RWMutex
	subs map[string]map[chan PushRow]struct{}
}

// newInProcessHub builds an empty in-process brain-keyed hub.
func newInProcessHub() *inProcessHub {
	return &inProcessHub{subs: map[string]map[chan PushRow]struct{}{}}
}

// fanoutBuffer is the per-subscriber channel depth. A subscriber that
// falls this far behind has rows dropped; the SSE reconnect backlog
// (since=<cursor>) replays them. Matches the previous hand-rolled depth.
const fanoutBuffer = 64

// Subscribe registers a new subscriber for brainID and returns its
// receive channel and an idempotent cancel func that unregisters it.
func (h *inProcessHub) Subscribe(brainID string) (<-chan PushRow, func()) {
	ch := make(chan PushRow, fanoutBuffer)
	h.mu.Lock()
	set := h.subs[brainID]
	if set == nil {
		set = map[chan PushRow]struct{}{}
		h.subs[brainID] = set
	}
	set[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			if set := h.subs[brainID]; set != nil {
				delete(set, ch)
				if len(set) == 0 {
					delete(h.subs, brainID)
				}
			}
			h.mu.Unlock()
		})
	}
	return ch, cancel
}

// Publish delivers row to every subscriber of brainID, dropping the row
// for any subscriber whose buffer is full (non-blocking). It never blocks
// the caller (the push handler) on a slow SSE client.
func (h *inProcessHub) Publish(brainID string, row PushRow) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[brainID] {
		select {
		case ch <- row:
		default:
			// Drop: subscriber is slow. SSE reconnect+since= replays it.
		}
	}
}
