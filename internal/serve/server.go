// Package serve implements the HTTP API behind `resleeve serve`. It's
// the synchronization endpoint clients push session/memory ciphertext
// to and pull from. The server is stateless: all persistence is
// delegated to a sync.Backend behind a stable wire format.
//
// **Zero-knowledge stance**: server never decrypts, server never
// inspects blob bytes. Auth gates access to the API; encryption gates
// access to content. See docs/design/round-4/02-cross-machine-sync.md.
//
// This is v2 slice 1: stub identity (single shared bearer token),
// push + pull + health endpoints, no SSE. Identity, SSE, and the
// kind→prefix mapping shape land in subsequent slices.
package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/mattkorwel/resleeve/internal/sync"
)

// Config bundles the inputs needed to construct a Server.
type Config struct {
	Backend   sync.Backend
	AuthToken string // required; the server rejects with 503 if empty
}

// Server is the HTTP handler exposed at /v2/sync/*.
type Server struct {
	mux       *http.ServeMux
	backend   sync.Backend
	authToken string
}

// New builds a Server. AuthToken must be non-empty (the safe default
// is "this server is unconfigured" — accidentally exposing a serve
// without auth would defeat the zero-knowledge stance).
func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("serve: nil backend")
	}
	if cfg.AuthToken == "" {
		return nil, errors.New("serve: empty AuthToken (configure --auth-token or RESLEEVE_SERVE_TOKEN)")
	}
	s := &Server{
		mux:       http.NewServeMux(),
		backend:   cfg.Backend,
		authToken: cfg.AuthToken,
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v2/sync/health", s.handleHealth)
	s.mux.HandleFunc("POST /v2/sync/push", s.requireAuth(s.handlePush))
	s.mux.HandleFunc("GET /v2/sync/pull", s.requireAuth(s.handlePull))
}

// ServeHTTP makes *Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// --- middleware ---

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	expected := "Bearer " + s.authToken
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if got == "" || got != expected {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
}

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "resleeve-serve", "version": "v2"})
}

// PushReq is the wire body for POST /v2/sync/push.
type PushReq struct {
	Batch []PushRow `json:"batch"`
}

// PushRow is one (key, blob) entry. Blob is base64 in JSON.
type PushRow struct {
	Key  string `json:"key"`
	Blob []byte `json:"blob"`
}

// PushResp acknowledges what got committed.
type PushResp struct {
	Committed []string `json:"committed"`
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req PushReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if len(req.Batch) == 0 {
		writeError(w, http.StatusBadRequest, "empty batch")
		return
	}
	committed := make([]string, 0, len(req.Batch))
	for _, row := range req.Batch {
		if err := s.backend.Put(r.Context(), row.Key, row.Blob); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("put %q: %v", row.Key, err))
			return
		}
		committed = append(committed, row.Key)
	}
	writeJSON(w, http.StatusOK, PushResp{Committed: committed})
}

// PullResp is the wire body for GET /v2/sync/pull.
type PullResp struct {
	Rows       []PushRow `json:"rows"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind == "" {
		writeError(w, http.StatusBadRequest, "missing required ?kind=")
		return
	}
	if err := validateKind(kind); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	since := q.Get("since")
	limit := parseLimit(q.Get("limit"), 100)

	keys, next, err := s.backend.List(r.Context(), kind, since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list: %v", err))
		return
	}
	rows := make([]PushRow, 0, len(keys))
	for _, k := range keys {
		blob, err := s.backend.Get(r.Context(), k)
		if err != nil {
			// A key that listed but failed to Get is a backend bug;
			// surface but continue so partial pagination still works.
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("get %q: %v", k, err))
			return
		}
		rows = append(rows, PushRow{Key: k, Blob: blob})
	}
	writeJSON(w, http.StatusOK, PullResp{Rows: rows, NextCursor: next})
}

// --- helpers ---

func validateKind(kind string) error {
	switch kind {
	case "sessions", "events", "memory":
		return nil
	default:
		return fmt.Errorf("invalid kind %q (expected sessions, events, or memory)", kind)
	}
}

func parseLimit(raw string, def int) int {
	if raw == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n <= 0 {
		return def
	}
	if n > 500 {
		return 500
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
