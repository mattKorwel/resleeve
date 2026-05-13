package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

func (d *Daemon) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/sessions/", d.handleSessionEvents)
}

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

// handleSessionEvents handles POST /v1/sessions/{id}/events.
// Stage 3a: auto-creates the session if missing with minimal metadata
// derived from the first event. Stage 3b will move session metadata
// onto a dedicated POST /v1/sessions endpoint and tighten auth.
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

	ctx := r.Context()

	// Auto-create session if missing (3a stub behavior).
	if _, err := d.store.Sessions().Get(ctx, sessionID); err != nil {
		if !errors.Is(err, rsql.ErrNotFound) {
			http.Error(w, "session get: "+err.Error(), http.StatusInternalServerError)
			return
		}
		first := body.Events[0]
		ses := &rsql.Session{
			ID:         sessionID,
			Slot:       first.Slot,
			CLI:        first.Vendor.Name,
			CLIVersion: first.Vendor.Version,
			StartedAt:  time.Now().UTC(),
			Status:     rsql.SessionStatusActive,
		}
		if err := d.store.Sessions().Create(ctx, ses); err != nil {
			http.Error(w, "session create: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := d.store.Events().Append(ctx, body.Events); err != nil {
		http.Error(w, "append: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
