# Round 13 — Slice 1: brain-keyed proactive fan-out + SSE hardening

Implements the round-13 requirement (`00-proactive-fanout-and-scale.md`): a
memory write to a shared brain **X** must reach the *other members of X*
proactively over SSE, not just on the slow periodic pull. Single-instance
(stage A on the scale ladder) only — the Postgres backplane is deferred.

## Starting point — what the fan-out did before this slice

The live SSE path was a **single global subscriber set** (`map[chan
PushRow]struct{}` on the `Server`). `handlePush` broadcast every memory row
to *every* SSE channel; each `handleSSE` connection then filtered by its own
brain prefix (`strings.HasPrefix(row.Key, brainID+"/")`) and dropped
non-matching rows.

Consequences:

- **Cross-member delivery already worked.** Member B subscribed to shared
  brain X *did* receive member A's push — the global broadcast reached B's
  channel and B's prefix filter matched. The fan-out was keyed by nothing
  (global), not by user, so it was never "pusher's own devices only."
- **But it was inefficient.** Every brain's write was copied into every open
  SSE channel across all brains and then dropped client-side (server-side, in
  the handler). A popular brain's writes amplified to unrelated connections.

This slice keeps the cross-member guarantee and removes the amplification by
**keying the fan-out hub on `brain_id`** so a push to X is delivered only to
subscribers whose acting brain is X.

## The brain-keyed fan-out hub (`internal/serve/fanout.go`)

A small in-process pub/sub behind an interface seam:

```go
type fanoutHub interface {
    Subscribe(brainID string) (<-chan PushRow, func())
    Publish(brainID string, row PushRow)
}
```

- `Subscribe(brainID)` registers a buffered receive channel under that brain
  and returns an idempotent `cancel` the caller defers to unregister.
- `Publish(brainID, row)` is **non-blocking**: a subscriber whose buffer
  (`fanoutBuffer = 64`) is full has the row dropped. Publish never blocks the
  push handler on a slow SSE client.

`inProcessHub` is the single-instance impl: `map[brainID]map[chan
PushRow]struct{}` guarded by an `sync.RWMutex`. Empty brain sets are pruned on
the last unsubscribe.

### Wiring

- `Server.fanout fanoutHub` replaces the old `sseMu` / `sseSubscribers`
  fields. `New` constructs `newInProcessHub()`.
- `handlePush`: for each committed `memory/` row, `s.fanout.Publish(bc.brainID,
  PushRow{Key: storedKey, Blob: row.Blob})` — published under the
  authenticated acting brain (never a client-supplied value), carrying the
  **stored (brain-prefixed) key** and the **plaintext blob** (matching what
  `pull` returns).
- `handleSSE`: `ch, cancel := s.fanout.Subscribe(bc.brainID); defer cancel()`.
  Because the hub is brain-keyed, the live loop receives *only* this brain's
  rows; it strips the brain prefix (`unscopeKey`) before writing the frame.
  The old per-row prefix re-check is gone (the routing now guarantees it).

### Correctness contract preserved (notify → pull)

SSE remains a **latency optimization over a correct pull baseline**:

- The backlog replay on connect (`List` strictly after `since`) still runs and
  still decrypts at-rest blobs with the brain DEK.
- The 30s periodic pull is the backstop. A dropped notify (full buffer, or a
  connection pinned to another instance pre-backplane) is caught by the next
  pull. Nothing in this slice depends on SSE for correctness.
- The live path still ships plaintext (push already had it); only the
  stored-read backlog branch decrypts. At-rest decrypt/strip behavior on the
  SSE path is unchanged.

## SSE hardening — heartbeats + reconnect

- **Heartbeats (server):** `handleSSE` emits an SSE comment `": ping\n\n"` on
  `Server.sseHeartbeat` (default 15s; tests override it). A leading-`:` line is
  a no-op for the client parser but keeps reverse proxies / LBs from closing
  the idle connection.
- **Reconnect/backoff (client):** `internal/agent` `sseLoop` already reconnects
  with exponential backoff (1s → 2s → … cap 30s), resetting on a clean EOF, and
  `runSSE` already ignores `:`-prefixed heartbeat lines and parks without a
  sealer. Reconnects pass `since=<cursor>` so rows missed during downtime are
  replayed by the server's backlog walk. No client change was required for the
  `": ping"` form — it matches the existing `strings.HasPrefix(line, ":")`
  branch.
- **Coalesce/debounce: not implemented.** Notifies are not debounced in this
  slice. Cursor pulls are cheap and the buffered channel + drop-on-full already
  bounds a thundering herd; debounce can be layered onto `Publish` later behind
  the same interface if a hot shared brain warrants it. Noted as optional in
  the spec.

## Tests (`internal/serve/fanout_test.go`)

All run under `go test -race`:

- `TestFanout_CrossMemberLiveDelivery` — **the core new guarantee**: owner A
  and member B both belong to shared brain X; B subscribes over SSE; A's push
  to X is delivered live to B.
- `TestFanout_IsolationNonMember` — a user subscribed to a *different* brain
  (their personal brain) receives nothing when the owner pushes to the shared
  brain they're not in.
- `TestFanout_PusherOwnOtherDevice` — A's own other device, subscribed to the
  same brain, still receives A's push (self-fan-out survives the brain-keying).
- `TestFanout_Heartbeat` — with a 50ms heartbeat interval, a `:`-comment
  keepalive arrives within the window.

The existing `TestSSE_BacklogThenLive` (single-tenant, global keyspace) still
passes — `brainID == ""` routes through the hub under the empty-string key, so
single-tenant behavior is unchanged.

## The seam the Postgres backplane slots into (deferred)

`fanoutHub` is the entire seam. The deferred scale-out slice replaces
`inProcessHub` with a `pgNotifyHub` implementing the same two methods:

- `Publish(brainID, row)` → `NOTIFY` on a channel derived from `brainID`
  (the row payload, or just a cursor bump, on the NOTIFY message).
- `Subscribe(brainID)` → a `LISTEN` on that channel plus a per-process relay
  goroutine that fans incoming notifications out to the local subscriber
  channels — so an SSE connection pinned to instance B receives a write that
  happened on instance A.

**No handler changes** are needed: `handlePush` and `handleSSE` already speak
only to the `fanoutHub` interface. NATS / Redis pub-sub sit behind the same
seam later if Postgres `NOTIFY` throughput is ever exceeded. Out of scope here
(per `00-…`): the Postgres backplane itself, Postgres serve-store / S3 blob
backends, and webhooks.
