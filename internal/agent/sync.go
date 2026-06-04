package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/event"
	"github.com/mattkorwel/resleeve/internal/memory"
	"github.com/mattkorwel/resleeve/internal/serve"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// SyncClient wires the local daemon to an upstream `resleeve serve`
// instance. It runs two background goroutines:
//
//   - drainLoop: every drainInterval, pull pending rows from the local
//     outbox and POST them to /v2/sync/push. Successfully-committed
//     rows are removed from the outbox; transient failures bump the
//     row's next_attempt_at via exponential backoff.
//
//   - pullLoop: every pullInterval, GET /v2/sync/pull?kind=... for
//     each synced kind, ingest the returned blobs into the local
//     SQLite (idempotent — sessions Create and events Append are
//     both INSERT OR IGNORE), and advance the per-kind cursor.
//
// Enqueue is called inline from IngestBatch after the local Append
// commits. Encryption is intentionally NOT yet applied — slice 2 wires
// the pipe; slice 2.5 inserts the KEK encryption layer at the
// Enqueue/ingestPulled boundary. See docs/design/round-4/02-cross-machine-sync.md.
type SyncClient struct {
	store    rsql.Store
	upstream string
	token    string
	httpc    *http.Client
	sseHTTPc *http.Client // separate client; no timeout (long-lived stream)

	drainInterval time.Duration
	pullInterval  time.Duration
	batchSize     int

	stop context.CancelFunc
	done chan struct{}

	// sealer wraps outbound blobs and unwraps inbound blobs at the
	// push/pull boundary. nil = plaintext mode (used by tests and by
	// daemons with no --upstream). See round-4/02-cross-machine-sync.md
	// §"Zero-knowledge layer".
	//
	// Post-#3: also nil when the daemon is up but the user hasn't run
	// `resleeve login` yet. The drain/pull/sse loops park while it's
	// nil; Enqueue is a no-op (capture stays local until login).
	// SetSealer installs one at runtime; ClearSealer locks it back to
	// nil. sealerMu guards reads + writes; the loops re-check on every
	// tick so a freshly installed sealer is picked up within parkInterval.
	sealerMu sync.RWMutex
	sealer   auth.Sealer

	// parkInterval is how often a parked loop wakes up to check whether
	// a sealer has been installed. Kept short for snappy login UX.
	parkInterval time.Duration

	// parkLogMu guards parkLogAt — used to throttle "waiting for login"
	// log lines to once per minute per loop.
	parkLogMu sync.Mutex
	parkLogAt map[string]time.Time

	// stats tracks runtime liveness for `resleeve doctor`. Mutated from
	// the drain/pull/sse loops via record* helpers (defined at the
	// bottom of this file) and read out via Snapshot() for the
	// /v1/doctor/sync-status endpoint. See round-4/03 §"resleeve doctor".
	stats syncStats
}

// NewSyncClient builds a client. upstream is the base URL of a
// `resleeve serve` instance (e.g. "https://sync.you.dev"). token is
// the bearer secret the server expects.
func NewSyncClient(store rsql.Store, upstream, token string) *SyncClient {
	return &SyncClient{
		store:         store,
		upstream:      strings.TrimRight(upstream, "/"),
		token:         token,
		httpc:         &http.Client{Timeout: 30 * time.Second},
		sseHTTPc:      &http.Client{}, // no timeout: SSE is long-lived; context controls lifetime
		drainInterval: 1 * time.Second,
		pullInterval:  30 * time.Second,
		parkInterval:  5 * time.Second,
		batchSize:     100,
		done:          make(chan struct{}, 3),
	}
}

// SetSealer installs (or replaces) the sealer used at the push/pull
// boundary. Called by the daemon's /v1/seal/unlock handler after
// `resleeve login` derives the KEK locally. Idempotent + concurrent-safe.
func (s *SyncClient) SetSealer(sl auth.Sealer) {
	s.sealerMu.Lock()
	s.sealer = sl
	s.sealerMu.Unlock()
}

// ClearSealer drops the installed sealer. Called by /v1/seal/lock on
// `resleeve logout`; the drain/pull/sse loops then park until the
// next SetSealer.
func (s *SyncClient) ClearSealer() {
	s.sealerMu.Lock()
	s.sealer = nil
	s.sealerMu.Unlock()
}

// getSealer is the read-side snapshot of the current sealer. Returns
// nil when none is installed (post-startup, pre-login state).
func (s *SyncClient) getSealer() auth.Sealer {
	s.sealerMu.RLock()
	defer s.sealerMu.RUnlock()
	return s.sealer
}

// NewSyncClientWithSealer is NewSyncClient plus a Sealer that wraps
// every outbox blob before push and unwraps every pulled blob before
// ingest. With a Sealer the server-side backend stores only opaque
// ciphertext — the zero-knowledge property of round-4/02.
func NewSyncClientWithSealer(store rsql.Store, upstream, token string, sealer auth.Sealer) *SyncClient {
	c := NewSyncClient(store, upstream, token)
	c.SetSealer(sealer)
	return c
}

// Start kicks off the background drain + pull loops. Idempotent — a
// second Start replaces the prior cancel func; tests assume one Start.
func (s *SyncClient) Start(ctx context.Context) {
	ctx, s.stop = context.WithCancel(ctx)
	go s.drainLoop(ctx)
	go s.pullLoop(ctx)
	go s.sseLoop(ctx)
}

// Stop signals the loops to exit and waits up to 3 seconds for all
// three (drain, pull, sse).
func (s *SyncClient) Stop() {
	if s.stop == nil {
		return
	}
	s.stop()
	timeout := time.After(3 * time.Second)
	for i := 0; i < 3; i++ {
		select {
		case <-s.done:
		case <-timeout:
			return
		}
	}
}

// EnqueueSession is called by IngestBatch after the local Sessions.Create
// commits. Idempotent on session ID (the outbox unique constraint on
// (kind, key) absorbs duplicate enqueues).
//
// Sealer semantics: a non-nil sealer wraps the blob before storage; nil
// sealer stores plaintext (slice-2-pre-2.5 mode and test mode). The
// drain loop is what blocks when no sealer is installed — at enqueue
// time we ALWAYS write to the outbox so capture doesn't lose events
// across a logout/login or daemon restart.
func (s *SyncClient) EnqueueSession(ctx context.Context, ses *rsql.Session) error {
	blob, err := json.Marshal(ses)
	if err != nil {
		return fmt.Errorf("sync: marshal session: %w", err)
	}
	if sl := s.getSealer(); sl != nil {
		blob, err = sl.Seal(blob)
		if err != nil {
			return fmt.Errorf("sync: seal session: %w", err)
		}
	}
	key := "sessions/" + ses.ID
	return s.store.Sync().EnqueueOutbox(ctx, "sessions", key, blob, time.Now().UTC())
}

// EnqueueEvents is called by IngestBatch after Events.Append commits.
// One outbox row per event; idempotent via the (kind, key) constraint.
// Key format: "events/<sid>/<padded-seq>-<event_uuid>" so lexicographic
// ordering matches (seq, uuid) ordering — see round-2/04-event-schema.md.
//
// sec-M4: the sealer is re-sampled INSIDE the loop on every event, not
// once at the top, so a concurrent ClearSealer (e.g. `resleeve logout`
// racing with an in-flight IngestBatch) is observed mid-batch. With the
// previous top-of-loop sample, events later in the same batch were
// sealed under the just-cleared KEK; the row would then park in the
// outbox unable to drain until the user logged back in with the same
// password. Re-sampling makes the user's lock intent take effect at
// per-event granularity. enqueue-still-happens semantics (events get
// queued even when no sealer is installed, to avoid losing capture) are
// preserved in both branches — see seal_handlers.go.
func (s *SyncClient) EnqueueEvents(ctx context.Context, events []event.Event) error {
	now := time.Now().UTC()
	for _, e := range events {
		blob, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("sync: marshal event %s: %w", e.EventUUID, err)
		}
		if sl := s.getSealer(); sl != nil {
			blob, err = sl.Seal(blob)
			if err != nil {
				return fmt.Errorf("sync: seal event %s: %w", e.EventUUID, err)
			}
		}
		key := fmt.Sprintf("events/%s/%020d-%s", e.SessionID, e.Seq, e.EventUUID)
		if err := s.store.Sync().EnqueueOutbox(ctx, "events", key, blob, now); err != nil {
			return fmt.Errorf("sync: enqueue event %s: %w", e.EventUUID, err)
		}
	}
	return nil
}

// PullNowCounts runs one immediate pull cycle and returns per-kind
// ingest counts. Used by on-demand callers (e.g. `resleeve resume`
// before resolving, the `resleeve sync pull` escape-hatch endpoint) to
// skip the periodic-pull wait. Counts are the number of rows
// successfully dispatched into the local stores during this call;
// idempotent re-ingests of already-known rows still count (we can't
// cheaply distinguish them here without per-store change probes).
// Callers that don't care about counts can discard the first return.
func (s *SyncClient) PullNowCounts(ctx context.Context) (map[string]int, error) {
	return s.pullAllKindsCounts(ctx)
}

// DrainNow drains the outbox in tight back-to-back batches until either
// (a) the outbox is empty, (b) a batch makes no progress (transient
// upstream failure — bumpAll already pushed the rows past
// next_attempt_at), or (c) maxBatches is reached as a safety cap.
// Returns the number of successfully-pushed rows.
//
// Used by the `resleeve sync push` escape-hatch endpoint when the
// caller wants the pipe drained NOW instead of waiting for the 1s
// drain ticker. Background drainLoop continues to tick independently.
func (s *SyncClient) DrainNow(ctx context.Context) (int, error) {
	const maxBatches = 100 // 100 * batchSize(100) = 10k row cap per call
	total := 0
	for i := 0; i < maxBatches; i++ {
		depthBefore, err := s.store.Sync().OutboxDepth(ctx)
		if err != nil {
			return total, err
		}
		if depthBefore == 0 {
			return total, nil
		}
		if err := s.drainOnce(ctx); err != nil {
			return total, err
		}
		depthAfter, err := s.store.Sync().OutboxDepth(ctx)
		if err != nil {
			return total, err
		}
		pushed := depthBefore - depthAfter
		if pushed <= 0 {
			// drainOnce ran but no rows came off the queue. Either there
			// were none ready (next_attempt_at gating) or upstream
			// rejected them and bumpAll pushed them past now+backoff.
			// Either way, we're done.
			return total, nil
		}
		total += pushed
	}
	return total, nil
}

// --- internal ---

func (s *SyncClient) drainLoop(ctx context.Context) {
	defer func() { s.done <- struct{}{} }()
	t := time.NewTicker(s.drainInterval)
	defer t.Stop()
	// Run an immediate drain on start so events captured before sync
	// was up don't wait a full interval. Park-on-nil-sealer applies to
	// the initial drain too; we log the wait once and let the timer
	// pick up the work when a sealer is installed.
	if !s.parked("drain") {
		if err := s.drainOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("sync drain (initial): %v", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.parked("drain") {
				continue
			}
			if err := s.drainOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("sync drain: %v", err)
			}
		}
	}
}

// parked returns true (and logs once-per-minute) when no sealer is
// installed. The loops use this as a soft pause: ticker fires on the
// normal cadence, but the work is skipped until SetSealer arrives.
// We don't lengthen the drain interval — outbox depth stays accurate
// for `doctor`, and the no-op cost is negligible.
func (s *SyncClient) parked(loop string) bool {
	if s.getSealer() != nil {
		return false
	}
	// Throttle the log so we don't spam every drainInterval tick.
	s.parkLogMu.Lock()
	defer s.parkLogMu.Unlock()
	if time.Since(s.parkLogAt[loop]) > time.Minute {
		log.Printf("sync %s: parked (waiting for resleeve login)", loop)
		if s.parkLogAt == nil {
			s.parkLogAt = map[string]time.Time{}
		}
		s.parkLogAt[loop] = time.Now()
	}
	return true
}

func (s *SyncClient) drainOnce(ctx context.Context) error {
	rows, err := s.store.Sync().DequeueOutbox(ctx, s.batchSize)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	batch := make([]serve.PushRow, len(rows))
	for i, r := range rows {
		batch[i] = serve.PushRow{Key: r.Key, Blob: r.Blob}
	}
	body, err := json.Marshal(serve.PushReq{Batch: batch})
	if err != nil {
		return fmt.Errorf("marshal push: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.upstream+"/v2/sync/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpc.Do(req)
	if err != nil {
		s.bumpAll(ctx, rows, err.Error())
		return fmt.Errorf("push http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		s.bumpAll(ctx, rows, fmt.Sprintf("status %d: %s", resp.StatusCode, string(errBody)))
		return fmt.Errorf("push status %d", resp.StatusCode)
	}
	var pushResp serve.PushResp
	if err := json.NewDecoder(resp.Body).Decode(&pushResp); err != nil {
		s.bumpAll(ctx, rows, fmt.Sprintf("decode response: %v", err))
		return err
	}
	keyToSeq := make(map[string]int64, len(rows))
	for _, r := range rows {
		keyToSeq[r.Key] = r.Seq
	}
	var ackSeqs []int64
	for _, k := range pushResp.Committed {
		if seq, ok := keyToSeq[k]; ok {
			ackSeqs = append(ackSeqs, seq)
		}
	}
	s.stats.recordDrain(time.Now().UTC())
	return s.store.Sync().AckOutbox(ctx, ackSeqs)
}

func (s *SyncClient) bumpAll(ctx context.Context, rows []rsql.OutboxRow, errMsg string) {
	for _, r := range rows {
		next := time.Now().Add(backoff(r.Attempts + 1))
		_ = s.store.Sync().BumpOutboxAttempt(ctx, r.Seq, next, errMsg)
	}
}

// backoff returns the wait before next attempt: 2s, 4s, 8s, 16s, ...
// capped at 5min.
func backoff(attempts int) time.Duration {
	d := time.Duration(1<<uint(attempts)) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func (s *SyncClient) pullLoop(ctx context.Context) {
	defer func() { s.done <- struct{}{} }()
	// Immediate pull on startup so a freshly-paired device starts
	// catching up without waiting an interval.
	if !s.parked("pull") {
		if err := s.pullAllKinds(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("sync pull (initial): %v", err)
		}
	}
	t := time.NewTicker(s.pullInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.parked("pull") {
				continue
			}
			if err := s.pullAllKinds(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("sync pull: %v", err)
			}
		}
	}
}

func (s *SyncClient) pullAllKinds(ctx context.Context) error {
	_, err := s.pullAllKindsCounts(ctx)
	return err
}

// pullAllKindsCounts runs the standard pull cycle and returns a
// per-kind count of successfully ingested rows. The background pullLoop
// uses pullAllKinds (counts discarded); the escape-hatch endpoint uses
// this variant so the CLI can print a summary.
func (s *SyncClient) pullAllKindsCounts(ctx context.Context) (map[string]int, error) {
	counts := make(map[string]int, 3)
	for _, kind := range []string{"sessions", "events", "memory"} {
		n, err := s.pullKind(ctx, kind)
		counts[kind] = n
		if err != nil {
			return counts, fmt.Errorf("pull %s: %w", kind, err)
		}
	}
	return counts, nil
}

func (s *SyncClient) pullKind(ctx context.Context, kind string) (int, error) {
	cursor, err := s.store.Sync().GetCursor(ctx, kind)
	if err != nil {
		return 0, err
	}
	ingested := 0
	for {
		u := fmt.Sprintf("%s/v2/sync/pull?kind=%s&since=%s&limit=500",
			s.upstream, kind, url.QueryEscape(cursor))
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return ingested, err
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		resp, err := s.httpc.Do(req)
		if err != nil {
			return ingested, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return ingested, fmt.Errorf("pull status %d", resp.StatusCode)
		}
		var pull serve.PullResp
		if err := json.NewDecoder(resp.Body).Decode(&pull); err != nil {
			resp.Body.Close()
			return ingested, err
		}
		resp.Body.Close()

		if len(pull.Rows) == 0 {
			break
		}
		for _, row := range pull.Rows {
			var ierr error
			if kind == "memory" {
				ierr = s.ingestMemoryRow(ctx, row.Key, row.Blob)
			} else {
				ierr = s.ingestPulled(ctx, kind, row.Blob)
			}
			if ierr != nil {
				log.Printf("sync: ingest %s/%s: %v", kind, row.Key, ierr)
				continue
			}
			cursor = row.Key
			ingested++
		}
		if cursor != "" {
			if err := s.store.Sync().SetCursor(ctx, kind, cursor); err != nil {
				return ingested, err
			}
		}
		if pull.NextCursor == "" {
			break
		}
	}
	s.stats.recordPull(kind, time.Now().UTC())
	return ingested, nil
}

func (s *SyncClient) ingestPulled(ctx context.Context, kind string, blob []byte) error {
	if sl := s.getSealer(); sl != nil {
		opened, err := sl.Open(blob)
		if err != nil {
			return fmt.Errorf("open envelope: %w", err)
		}
		blob = opened
	}
	switch kind {
	case "sessions":
		var ses rsql.Session
		if err := json.Unmarshal(blob, &ses); err != nil {
			return fmt.Errorf("unmarshal session: %w", err)
		}
		return s.store.Sessions().Create(ctx, &ses)
	case "events":
		var e event.Event
		if err := json.Unmarshal(blob, &e); err != nil {
			return fmt.Errorf("unmarshal event: %w", err)
		}
		if err := s.store.Events().Append(ctx, []event.Event{e}); err != nil {
			return fmt.Errorf("append event: %w", err)
		}
		return s.store.Sessions().SyncEventCount(ctx, e.SessionID)
	case "memory":
		// Memory keys are kind/<resource>/... but the SyncStore only
		// hands us blobs by outer kind, not the resource sub-prefix.
		// We dispatch by inspecting the unmarshaled shape via the
		// SSE/pull paths that know the key — see ingestMemoryRow.
		return errors.New("memory ingest requires key context — call ingestMemoryRow")
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
}

// ingestMemoryRow dispatches a pulled or SSE-delivered memory row to
// the right local store call based on the key shape:
//
//	memory/<encoded-path>                   -> UpdateScope
//	memory/<encoded-path>/plans/<slot>      -> PutPlan
//	memory/<encoded-path>/learnings/<id>    -> AppendLearning
//
// This shape is intentional: for any single scope path P, lexicographic
// order gives memory/P < memory/P/learnings/... < memory/P/plans/...,
// so the scope row is always applied before its child plan/learning
// rows. That satisfies the local SQLite FK constraint on
// (plans|learnings).scope -> scopes.path without needing in-memory
// reordering. See docs/design/round-4/02 for the design preview.
//
// All operations are idempotent against the local memory store, so
// replaying the same row (e.g. SSE backlog + live) is safe.
func (s *SyncClient) ingestMemoryRow(ctx context.Context, key string, blob []byte) error {
	if sl := s.getSealer(); sl != nil {
		opened, err := sl.Open(blob)
		if err != nil {
			return fmt.Errorf("sync: open memory blob %q: %w", key, err)
		}
		blob = opened
	}
	rest := strings.TrimPrefix(key, "memory/")
	if rest == key {
		return fmt.Errorf("not a memory key: %q", key)
	}
	switch {
	case strings.Contains(rest, "/plans/"):
		var plan memory.Plan
		if err := json.Unmarshal(blob, &plan); err != nil {
			return fmt.Errorf("unmarshal plan: %w", err)
		}
		return s.store.Memory().PutPlan(ctx, &plan)
	case strings.Contains(rest, "/learnings/"):
		var l memory.Learning
		if err := json.Unmarshal(blob, &l); err != nil {
			return fmt.Errorf("unmarshal learning: %w", err)
		}
		return s.store.Memory().AppendLearning(ctx, &l)
	default:
		// No sub-resource segment: this is the scope row itself.
		var scope memory.Scope
		if err := json.Unmarshal(blob, &scope); err != nil {
			return fmt.Errorf("unmarshal scope: %w", err)
		}
		// UpdateScope is upsert-like in the sqlite impl (INSERTs if
		// absent). Use it so we don't need GET-then-create-vs-update.
		return s.store.Memory().UpdateScope(ctx, &scope)
	}
}

// --- memory enqueue helpers (called by daemon memory handlers) ---

// encodeScopePath produces the on-the-wire scope segment for memory
// keys. We swap '/' for ':' so the scope path itself doesn't introduce
// new key segments (which the backend would treat as directory levels).
func encodeScopePath(p string) string {
	return strings.ReplaceAll(p, "/", ":")
}

// EnqueueScope enqueues a memory scope write to the outbox. Key shape:
// memory/<encoded-path>. Blob is the JSON scope as stored locally
// (read back via GetScope so created_at/updated_at reflect the commit).
func (s *SyncClient) EnqueueScope(ctx context.Context, scope *memory.Scope) error {
	blob, err := json.Marshal(scope)
	if err != nil {
		return fmt.Errorf("sync: marshal scope: %w", err)
	}
	if sl := s.getSealer(); sl != nil {
		blob, err = sl.Seal(blob)
		if err != nil {
			return fmt.Errorf("sync: seal scope: %w", err)
		}
	}
	key := "memory/" + encodeScopePath(scope.Path)
	return s.store.Sync().EnqueueOutbox(ctx, "memory", key, blob, time.Now().UTC())
}

// EnqueuePlan enqueues a plan write. Key shape:
// memory/<encoded-scope>/plans/<slot>. The plans/ sub-segment sorts
// AFTER the scope's own key (which has no extra segment), so a fresh
// machine reading the memory prefix in lexicographic order applies the
// scope before any of its plans (FK-safe).
func (s *SyncClient) EnqueuePlan(ctx context.Context, plan *memory.Plan) error {
	blob, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("sync: marshal plan: %w", err)
	}
	if sl := s.getSealer(); sl != nil {
		blob, err = sl.Seal(blob)
		if err != nil {
			return fmt.Errorf("sync: seal plan: %w", err)
		}
	}
	key := "memory/" + encodeScopePath(plan.Scope) + "/plans/" + plan.Name
	return s.store.Sync().EnqueueOutbox(ctx, "memory", key, blob, time.Now().UTC())
}

// EnqueueLearning enqueues a learning append. Key shape:
// memory/<encoded-scope>/learnings/<learning-id>. The learning ID is
// already monotonic-ish (stamp + random) so lexicographic order on the
// trailing segment matches creation order well enough for cursor
// pagination.
func (s *SyncClient) EnqueueLearning(ctx context.Context, l *memory.Learning) error {
	blob, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("sync: marshal learning: %w", err)
	}
	if sl := s.getSealer(); sl != nil {
		blob, err = sl.Seal(blob)
		if err != nil {
			return fmt.Errorf("sync: seal learning: %w", err)
		}
	}
	key := "memory/" + encodeScopePath(l.Scope) + "/learnings/" + l.ID
	return s.store.Sync().EnqueueOutbox(ctx, "memory", key, blob, time.Now().UTC())
}

// --- SSE consumer loop (fast tier) ---

// sseLoop holds an SSE connection to upstream's GET /v2/sync/sse?kind=memory
// and ingests live memory rows. On disconnect it reconnects with
// exponential backoff (1s, 2s, 4s, ... cap 30s). Reconnects pass
// since=<cursor> so missed rows during downtime are replayed by the
// server's backlog walk.
func (s *SyncClient) sseLoop(ctx context.Context) {
	defer func() { s.done <- struct{}{} }()
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// Park: SSE is a long-lived connection that decrypts each frame,
		// so without a sealer there's nothing useful to do. Sleep for
		// parkInterval and re-check; SetSealer will let the next pass
		// through.
		if s.parked("sse") {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.parkInterval):
				continue
			}
		}
		err := s.runSSE(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("sync sse: %v (reconnect in %s)", err, backoff)
		} else if err == nil {
			// Clean EOF: reset backoff since the server cleanly closed.
			backoff = 1 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// runSSE opens one SSE connection and ingests events until EOF/error.
// Each successful ingestion advances the memory cursor so a subsequent
// reconnect picks up exactly where we left off.
func (s *SyncClient) runSSE(ctx context.Context) error {
	cursor, err := s.store.Sync().GetCursor(ctx, "memory")
	if err != nil {
		return fmt.Errorf("get cursor: %w", err)
	}
	u := fmt.Sprintf("%s/v2/sync/sse?kind=memory&since=%s",
		s.upstream, url.QueryEscape(cursor))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := s.sseHTTPc.Do(req)
	if err != nil {
		return fmt.Errorf("sse dial: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sse status %d", resp.StatusCode)
	}
	s.stats.recordSSEConnected(time.Now().UTC())
	defer s.stats.recordSSEDisconnected()

	// SSE event framing per spec: lines separated by '\n'; an empty
	// line dispatches the buffered event. We only emit single-line
	// "data:" frames server-side, so a simple line-oriented parser
	// suffices.
	scanner := bufio.NewScanner(resp.Body)
	// Allow large blobs (memory plans / learnings can be sizeable
	// markdown). Default 64K is too tight for some pasted designs.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var dataBuf strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			// Dispatch: flush whatever's in dataBuf.
			if dataBuf.Len() == 0 {
				continue
			}
			payload := dataBuf.String()
			dataBuf.Reset()
			var row serve.PushRow
			if err := json.Unmarshal([]byte(payload), &row); err != nil {
				log.Printf("sync sse: malformed event payload: %v", err)
				continue
			}
			if err := s.ingestMemoryRow(ctx, row.Key, row.Blob); err != nil {
				log.Printf("sync sse: ingest %s: %v", row.Key, err)
				continue
			}
			s.stats.recordSSEEvent(time.Now().UTC())
			if err := s.store.Sync().SetCursor(ctx, "memory", row.Key); err != nil {
				log.Printf("sync sse: set cursor: %v", err)
			}
		case strings.HasPrefix(line, ":"):
			// SSE comment / heartbeat. Ignore.
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimPrefix(line, "data:")
			payload = strings.TrimPrefix(payload, " ")
			dataBuf.WriteString(payload)
		default:
			// event: / id: / retry: fields — unused here.
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("sse read: %w", err)
	}
	return nil
}

// --- runtime stats (read by /v1/doctor/sync-status) ---
//
// syncStats holds the runtime liveness fields surfaced by `resleeve
// doctor`. All accessors are mutex-protected so the doctor endpoint can
// snapshot a consistent view while the drain/pull/sse loops are
// concurrently recording new timestamps. Time fields are stored in
// UTC; zero value = "never happened".
//
// Why a sub-struct rather than per-field atomics on SyncClient: the
// snapshot wants a single point-in-time view across all fields, which
// a single mutex gives us for free. The mutations are infrequent (once
// per drain interval, once per pull interval, once per SSE event), so
// lock contention is not a concern. See round-4/03 §"resleeve doctor".
type syncStats struct {
	mu              sync.Mutex
	drainLast       time.Time
	pullLastPerKind map[string]time.Time
	sseConnected    bool
	sseConnectedAt  time.Time
	sseLastEventAt  time.Time
}

func (s *syncStats) recordDrain(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drainLast = t
}

func (s *syncStats) recordPull(kind string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pullLastPerKind == nil {
		s.pullLastPerKind = make(map[string]time.Time)
	}
	s.pullLastPerKind[kind] = t
}

func (s *syncStats) recordSSEConnected(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sseConnected = true
	s.sseConnectedAt = t
}

func (s *syncStats) recordSSEDisconnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sseConnected = false
	// Keep sseConnectedAt and sseLastEventAt for post-mortem visibility.
}

func (s *syncStats) recordSSEEvent(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sseLastEventAt = t
}

// SyncStatusSnapshot is the point-in-time view returned by
// /v1/doctor/sync-status. JSON-friendly; zero-time fields serialize
// as RFC3339 "0001-01-01T00:00:00Z" which the CLI side treats as
// "never". OutboxDepth and UpstreamReachable are filled by the
// daemon's handler (not the SyncClient) because they require a
// store query and a network probe respectively.
type SyncStatusSnapshot struct {
	DrainLast        time.Time            `json:"drain_last"`
	PullLastPerKind  map[string]time.Time `json:"pull_last_per_kind"`
	SSEConnected     bool                 `json:"sse_connected"`
	SSEConnectedAt   time.Time            `json:"sse_connected_at"`
	SSEUptimeSec     int64                `json:"sse_uptime_sec"`
	SSELastEventAt   time.Time            `json:"sse_last_event_at"`
	OutboxDepth      int                  `json:"outbox_depth"`
	UpstreamConfig   string               `json:"upstream"`
	UpstreamOK       bool                 `json:"upstream_ok"`
	UpstreamRTTms    int64                `json:"upstream_rtt_ms"`
	UpstreamError    string               `json:"upstream_error,omitempty"`
}

// Snapshot returns a copy of the stats (without OutboxDepth, upstream
// fields, or upstream config — those are filled by the daemon handler).
// Safe to call concurrently with the drain/pull/sse loops.
func (s *SyncClient) Snapshot() SyncStatusSnapshot {
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()
	out := SyncStatusSnapshot{
		DrainLast:      s.stats.drainLast,
		SSEConnected:   s.stats.sseConnected,
		SSEConnectedAt: s.stats.sseConnectedAt,
		SSELastEventAt: s.stats.sseLastEventAt,
	}
	if s.stats.sseConnected && !s.stats.sseConnectedAt.IsZero() {
		out.SSEUptimeSec = int64(time.Since(s.stats.sseConnectedAt).Seconds())
	}
	if len(s.stats.pullLastPerKind) > 0 {
		out.PullLastPerKind = make(map[string]time.Time, len(s.stats.pullLastPerKind))
		for k, v := range s.stats.pullLastPerKind {
			out.PullLastPerKind[k] = v
		}
	}
	return out
}

// UpstreamURL returns the configured upstream base URL (already
// trimmed of trailing slashes). Empty when no --upstream was passed.
// Used by the doctor handler to ping /v2/sync/health.
func (s *SyncClient) UpstreamURL() string {
	return s.upstream
}

// UpstreamToken returns the bearer secret sent to upstream. Used by
// the doctor handler to authenticate the /v2/sync/health probe.
func (s *SyncClient) UpstreamToken() string {
	return s.token
}
