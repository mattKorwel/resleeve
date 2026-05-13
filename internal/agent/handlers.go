package agent

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mattkorwel/resleeve/internal/event"
)

func (d *Daemon) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/sessions/", d.requireBearer(d.handleSessionEvents))
}

// handleHealth is unauthenticated; useful for `resleeve doctor` and
// liveness probes.
func (d *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"version": "0.0.0-dev",
	})
}

// handleSessionEvents handles POST /v1/sessions/{id}/events. The
// daemon ingests the batch via IngestBatch, which auto-creates the
// session if missing and heartbeats the slot.
func (d *Daemon) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v1/sessions/"
	const suffix = "/events"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
	if sessionID == "" {
		http.Error(w, "empty session id", http.StatusBadRequest)
		return
	}

	var body struct {
		Events []event.Event `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Events) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if err := d.IngestBatch(r.Context(), sessionID, body.Events); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// requireBearer is the authentication middleware. Compares the
// Authorization header's Bearer value to d.secret in constant time.
// Returns 401 on mismatch.
func (d *Daemon) requireBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		provided := strings.TrimPrefix(h, prefix)
		if subtle.ConstantTimeCompare([]byte(provided), []byte(d.secret)) != 1 {
			http.Error(w, "bad bearer", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// decodeIgnoringUnknown is a small helper for parsing partial JSON
// payloads where we only care about a few fields.
func decodeIgnoringUnknown(data []byte, target any) error {
	return json.Unmarshal(data, target)
}
