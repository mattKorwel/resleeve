package agent

import (
	"net/http"
)

// registerSyncRoutes wires the manual escape-hatch endpoints used by
// `resleeve sync push` and `resleeve sync pull`. Both routes require
// the bearer token and refuse with 409 if the daemon has no upstream
// configured — there's nothing for them to do without one.
func (d *Daemon) registerSyncRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/sync/push-now", d.requireBearer(d.handleSyncPushNow))
	mux.HandleFunc("/v1/sync/pull-now", d.requireBearer(d.handleSyncPullNow))
}

// handleSyncPushNow drains the outbox immediately and reports how many
// rows were pushed. Returns 409 if the daemon is running without an
// --upstream configured (no SyncClient to drive). See round-4/03
// §"New CLI verbs".
func (d *Daemon) handleSyncPushNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if d.sync == nil {
		// Q2: explicit no_upstream code so the client surfaces this as
		// ErrNoUpstream via errors.Is — no body-substring matching.
		writeErrorEnvelope(w, http.StatusConflict, codeNoUpstream,
			"no upstream configured — set --upstream on `resleeve up`")
		return
	}
	pushed, err := d.sync.DrainNow(r.Context())
	resp := map[string]any{"pushed": pushed}
	if err != nil {
		// Surface the error but keep the count: drain may have
		// pushed some rows before hitting a network blip.
		resp["error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSyncPullNow runs one immediate pull cycle and reports per-kind
// ingest counts. Returns 409 if no upstream is configured.
func (d *Daemon) handleSyncPullNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if d.sync == nil {
		// Q2: see handleSyncPushNow — same no_upstream envelope.
		writeErrorEnvelope(w, http.StatusConflict, codeNoUpstream,
			"no upstream configured — set --upstream on `resleeve up`")
		return
	}
	counts, err := d.sync.PullNowCounts(r.Context())
	resp := map[string]any{"pulled": counts}
	if err != nil {
		// Keep the partial counts; surface the error string.
		resp["error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
