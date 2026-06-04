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
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
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
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
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
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
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
			writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	// /v1/sessions/{id}
	if !strings.Contains(rest, "/") {
		if r.Method != http.MethodGet {
			writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		d.getSession(w, r, rest)
		return
	}
	http.NotFound(w, r)
}

func (d *Daemon) appendEvents(w http.ResponseWriter, r *http.Request, sessionID string) {
	if sessionID == "" {
		writeErrorStatus(w, http.StatusBadRequest, "empty session id")
		return
	}
	var body struct {
		Events []event.Event `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	if len(body.Events) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := d.IngestBatch(r.Context(), sessionID, body.Events); err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (d *Daemon) listEvents(w http.ResponseWriter, r *http.Request, sessionID string) {
	if sessionID == "" {
		writeErrorStatus(w, http.StatusBadRequest, "empty session id")
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
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
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
			writeErrorStatus(w, http.StatusNotFound, "not found")
			return
		}
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ses)
}

func (d *Daemon) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	query := q.Get("q")
	if query == "" {
		writeErrorStatus(w, http.StatusBadRequest, "missing q")
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
		writeErrorStatus(w, http.StatusInternalServerError, err.Error())
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
			writeErrorStatus(w, http.StatusUnauthorized, "missing bearer")
			return
		}
		provided := strings.TrimPrefix(h, prefix)
		if subtle.ConstantTimeCompare([]byte(provided), []byte(d.secret)) != 1 {
			writeErrorStatus(w, http.StatusUnauthorized, "bad bearer")
			return
		}
		next(w, r)
	}
}

