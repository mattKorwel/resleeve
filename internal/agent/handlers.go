package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/event"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

func (d *Daemon) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/health", d.handleHealth)
	mux.HandleFunc("/v1/sessions", d.requireBearer(d.handleSessionsCollection))
	mux.HandleFunc("/v1/sessions/", d.requireBearer(d.handleSessionsItem))
	mux.HandleFunc("/v1/search", d.requireBearer(d.handleSearch))
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

// handleSessionsCollection handles GET /v1/sessions — list with filters.
func (d *Daemon) handleSessionsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	f := rsql.SessionFilter{
		Scope:     q.Get("scope"),
		AgentName: q.Get("agent_name"),
	}
	if s := q.Get("status"); s != "" {
		f.Status = rsql.SessionStatus(s)
	}
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			f.Limit = n
		}
	}
	if s := q.Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			f.Since = &t
		}
	}

	sessions, err := d.store.Sessions().List(r.Context(), f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []*rsql.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// handleSessionsItem routes /v1/sessions/{id}, /v1/sessions/{id}/events.
func (d *Daemon) handleSessionsItem(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v1/sessions/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" {
		http.NotFound(w, r)
		return
	}

	// /v1/sessions/{id}/events
	if strings.HasSuffix(rest, "/events") {
		sessionID := strings.TrimSuffix(rest, "/events")
		switch r.Method {
		case http.MethodPost:
			d.appendEvents(w, r, sessionID)
		case http.MethodGet:
			d.listEvents(w, r, sessionID)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// /v1/sessions/{id}
	if !strings.Contains(rest, "/") {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		d.getSession(w, r, rest)
		return
	}
	http.NotFound(w, r)
}

func (d *Daemon) appendEvents(w http.ResponseWriter, r *http.Request, sessionID string) {
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

func (d *Daemon) listEvents(w http.ResponseWriter, r *http.Request, sessionID string) {
	if sessionID == "" {
		http.Error(w, "empty session id", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	sinceSeq := int64(0)
	if v := q.Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			sinceSeq = n
		}
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	events, err := d.store.Events().List(r.Context(), sessionID, sinceSeq, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []event.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (d *Daemon) getSession(w http.ResponseWriter, r *http.Request, id string) {
	ses, err := d.store.Sessions().Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, rsql.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, ses)
}

func (d *Daemon) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	query := q.Get("q")
	if query == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	hits, err := d.store.Events().Search(r.Context(), query, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if hits == nil {
		hits = []rsql.EventSearchHit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
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

