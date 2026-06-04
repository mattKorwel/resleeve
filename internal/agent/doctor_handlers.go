package agent

import (
	"context"
	"net/http"
	"time"
)

// registerDoctorRoutes mounts the /v1/doctor/* endpoints. Called from
// daemon.New alongside the main and memory route registrations.
//
// Why a separate function (vs. inlining into registerRoutes): the
// doctor surface is read-only introspection — keeping it in its own
// file makes it obvious which handlers can safely be hit from local
// tooling without risk of mutating state.
func (d *Daemon) registerDoctorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/doctor/sync-status", d.requireBearer(d.handleDoctorSyncStatus))
}

// handleDoctorSyncStatus returns a SyncStatusSnapshot. When no
// upstream is configured (d.sync == nil) the daemon still returns
// 200 with a snapshot whose UpstreamConfig is empty — the CLI side
// renders that as "standalone — no upstream configured".
//
// When upstream IS configured this handler also probes the upstream's
// /v2/sync/health endpoint and reports RTT or the error. The probe
// has a hard 3s timeout so a flaky upstream doesn't hang `doctor`.
// Probing here (rather than in the CLI) keeps the CLI thin and avoids
// duplicating the bearer-token plumbing in two places.
func (d *Daemon) handleDoctorSyncStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	// Outbox depth is always available (the local store is always open).
	depth, err := d.store.Sync().OutboxDepth(ctx)
	if err != nil {
		http.Error(w, "outbox depth: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if d.sync == nil {
		// Standalone daemon: no SyncClient means no drain/pull/sse
		// state to report. Return a snapshot with just outbox depth
		// + empty upstream so the CLI prints the "standalone" card.
		writeJSON(w, http.StatusOK, SyncStatusSnapshot{
			OutboxDepth: depth,
		})
		return
	}

	snap := d.sync.Snapshot()
	snap.OutboxDepth = depth
	snap.UpstreamConfig = d.sync.UpstreamURL()
	if snap.UpstreamConfig != "" {
		snap.UpstreamOK, snap.UpstreamRTTms, snap.UpstreamError = probeUpstream(ctx, snap.UpstreamConfig, d.sync.UpstreamToken())
	}
	writeJSON(w, http.StatusOK, snap)
}

// probeUpstream does a single GET to <upstream>/v2/sync/health and
// reports OK + round-trip time (ms) or the error. Hard 3s timeout so
// `doctor` never hangs. The token is sent as Bearer even though
// /v2/sync/health is unauthenticated server-side; if a future change
// adds auth there, this probe keeps working.
func probeUpstream(ctx context.Context, base, token string) (ok bool, rttMs int64, errMsg string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/sync/health", nil)
	if err != nil {
		return false, 0, err.Error()
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return false, 0, err.Error()
	}
	defer resp.Body.Close()
	rtt := time.Since(start).Milliseconds()
	if resp.StatusCode != http.StatusOK {
		return false, rtt, http.StatusText(resp.StatusCode)
	}
	return true, rtt, ""
}
