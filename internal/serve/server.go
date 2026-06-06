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
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
	rsync "github.com/mattkorwel/resleeve/internal/sync"
)

// Config bundles the inputs needed to construct a Server.
//
// Two auth modes coexist:
//   - AuthToken (legacy): a single shared bearer presented by every
//     client. Pre-#3 stub identity. Tests still use this.
//   - ServerUsers + Devices + Pairings (post-#3): per-device tokens
//     persisted in a database. When these are set, /v2/auth/* routes
//     light up and /v2/sync/* accepts EITHER a per-device bearer OR
//     the legacy AuthToken (for migration overlap).
//
// At least one of (AuthToken, Devices) must be set or New returns an
// error — accidentally exposing a serve without auth would defeat the
// zero-knowledge stance.
type Config struct {
	Backend     rsync.Backend
	AuthToken   string // legacy single bearer; empty when only per-device auth is desired
	ServerUsers rsql.ServerUserStore
	Devices     rsql.DeviceStore
	Pairings    rsql.PairingStore
	// ServeMeta persists process-level secrets across restart (sec-M1).
	// When nil, the server falls back to a per-process random
	// login-challenge HMAC key (the legacy AuthToken-only mode). When
	// set, the key is generated on first boot and persisted, so the
	// synthetic salt returned for unknown-email login-challenge probes
	// is stable across `resleeve serve` reboots — removing the "did the
	// server restart?" oracle a passive observer could otherwise mount.
	ServeMeta rsql.ServeMetaStore
	// Brains + Memberships are the round-11 multi-tenant foundation.
	// When both are set, register auto-provisions a personal brain (and
	// the registering user's membership of it). Keyspace partitioning and
	// authz that consume these land in later slices. When nil, register
	// still succeeds but provisions no brain (legacy single-user mode).
	Brains      rsql.BrainStore
	Memberships rsql.MembershipStore
}

// Server is the HTTP handler exposed at /v2/sync/* and /v2/auth/*.
type Server struct {
	mux         *http.ServeMux
	backend     rsync.Backend
	authToken   string
	serverUsers rsql.ServerUserStore
	devices     rsql.DeviceStore
	pairings    rsql.PairingStore
	brains      rsql.BrainStore
	memberships rsql.MembershipStore

	// loginChallengeKey is a persisted random secret used to derive
	// synthetic Argon2id salts for unknown-email login-challenge requests
	// (HMAC(key, "login-challenge-salt-v1"||email) → 16-byte salt). This
	// keeps the response shape indistinguishable from a real account so
	// /v2/auth/login-challenge cannot be used to enumerate registered
	// emails. sec-M1: the key is stored in serve_meta (when ServeMeta is
	// configured) and never rotated, so two probes of the same unknown
	// email across a restart return the same decoy salt — closing the
	// "did the server restart?" oracle that a per-process key gave a
	// passive observer. When ServeMeta is nil (legacy AuthToken-only
	// mode), the key is per-process random; that mode is on the way out
	// and pre-dates the identity stack the oracle would target.
	loginChallengeKey []byte

	// sseSubscribers is the live SSE fan-out set for kind=memory.
	// Each subscriber owns a buffered channel; slow subscribers get
	// their events dropped (the SSE backlog replay covers catch-up).
	sseMu          sync.RWMutex
	sseSubscribers map[chan PushRow]struct{}

	// loginTimingDecoy is a per-process random 32-byte value compared
	// against the client's verifier_hash on the unknown-email branch of
	// /v2/auth/login (sec-M6). Without it, the unknown-email path skips
	// the subtle.ConstantTimeCompare the known-email path runs, giving a
	// careful attacker a timing-distinguisher between "no such email" and
	// "wrong password" — small (the compare is sub-microsecond next to a
	// DB round trip) but cleanly distinct as an early-return shape. The
	// decoy never matches a real verifier (random), so the dummy compare
	// always returns 0 and the handler still 401s.
	loginTimingDecoy []byte

	// pairAttemptsMu + pairAttempts is the in-memory failed-claim counter
	// for /v2/auth/pair/claim (sec-H2). Keyed by code_id. The 60-bit pair
	// code is brute-forceable within its 5-min TTL if code_id leaks, so
	// after pairClaimMaxAttempts consecutive verifier failures we hard-
	// delete the code row and clear the counter — subsequent claims see
	// the same ErrNotFound as a swept-expired code, making lockout
	// indistinguishable from TTL expiry on the wire. State is purely
	// in-memory: lost on restart, but codes are 5-min TTL anyway and the
	// SweepExpired/Get paths reconcile any drift. Map entries are cleaned
	// up lazily on successful claim or on the lockout-delete path.
	pairAttemptsMu sync.Mutex
	pairAttempts   map[string]int
}

// pairClaimMaxAttempts is the per-code consecutive failed-verifier
// threshold before /v2/auth/pair/claim hard-deletes the pairing row
// (sec-H2). Five is a balance between human-typo tolerance (an accepter
// fat-fingering the pair code once or twice during the 5-minute TTL
// shouldn't lock them out) and the brute-force cost we're imposing: at
// 5 attempts, the expected work to guess a 60-bit code is 2^59 / 5 ≈
// 2^57 lockout cycles, each requiring a fresh publish from the inviter.
const pairClaimMaxAttempts = 5

// New builds a Server. At least one auth mode (legacy AuthToken or
// per-device Devices store) must be configured; without either, New
// errors. Mixing both is supported for migration overlap.
func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("serve: nil backend")
	}
	if cfg.AuthToken == "" && cfg.Devices == nil {
		return nil, errors.New("serve: no auth configured (set AuthToken or Devices)")
	}
	// sec-M1: persist the login-challenge HMAC key so the synthetic salt
	// for unknown-email probes is stable across `resleeve serve` restarts.
	// First-boot generates 32 random bytes; subsequent boots re-read the
	// same value from serve_meta. When ServeMeta is unconfigured (legacy
	// AuthToken-only mode without an identity store), we fall back to a
	// per-process key — pre-identity deployments don't run the synthetic-
	// salt code path on registered users so the oracle doesn't bite there.
	challengeKey, err := loadOrCreateLoginChallengeKey(cfg.ServeMeta)
	if err != nil {
		return nil, fmt.Errorf("serve: load login-challenge key: %w", err)
	}
	// sec-M6 decoy: per-process random, never persisted — its only job
	// is to give the unknown-email login branch the same compare cost
	// as the known-email branch.
	timingDecoy := make([]byte, 32)
	if _, err := rand.Read(timingDecoy); err != nil {
		return nil, fmt.Errorf("serve: gen login-timing decoy: %w", err)
	}
	s := &Server{
		mux:               http.NewServeMux(),
		backend:           cfg.Backend,
		authToken:         cfg.AuthToken,
		serverUsers:       cfg.ServerUsers,
		devices:           cfg.Devices,
		pairings:          cfg.Pairings,
		brains:            cfg.Brains,
		memberships:       cfg.Memberships,
		loginChallengeKey: challengeKey,
		loginTimingDecoy:  timingDecoy,
		sseSubscribers:    map[chan PushRow]struct{}{},
		pairAttempts:      map[string]int{},
	}
	s.routes()
	if cfg.ServerUsers != nil && cfg.Devices != nil {
		s.registerAuthRoutes()
	}
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v2/sync/health", s.handleHealth)
	s.mux.HandleFunc("POST /v2/sync/push", s.requireAuth(s.handlePush))
	s.mux.HandleFunc("GET /v2/sync/pull", s.requireAuth(s.handlePull))
	s.mux.HandleFunc("GET /v2/sync/sse", s.requireAuth(s.handleSSE))
}

// ServeHTTP makes *Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// --- middleware ---

// requireAuth accepts EITHER a per-device bearer (looked up in the
// devices table) OR the legacy single-bearer AuthToken if configured.
// The shared deviceFromBearer helper handles both. We deliberately
// don't surface the device to /v2/sync/* handlers — the sync routes
// are content-blind (server stores opaque ciphertext keyed by client),
// so per-device scoping doesn't apply there yet. That ships when we
// move to per-user backend partitions in a follow-up slice.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.deviceFromBearer(r); !ok {
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
		// sec-M2: gate the key prefix so a misbehaving or compromised
		// client can't write blobs under arbitrary prefixes (disk fill /
		// blob smuggling). The three allowed prefixes mirror the wire
		// kinds validated by validateKind on the pull/SSE side; keep them
		// in sync if a new kind ships.
		if !isAllowedPushKey(row.Key) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("rejected key %q: must start with sessions/, events/, or memory/", row.Key))
			return
		}
		if err := s.backend.Put(r.Context(), row.Key, row.Blob); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("put %q: %v", row.Key, err))
			return
		}
		committed = append(committed, row.Key)
		// Fast-tier fan-out: memory rows trigger live delivery to SSE
		// subscribers. Backend.Put has already persisted; SSE is just
		// the low-latency notification path. Slow subscribers (full
		// buffer) silently drop — they'll catch up on reconnect via
		// the backlog replay window.
		if strings.HasPrefix(row.Key, "memory/") {
			s.fanoutMemory(row)
		}
	}
	writeJSON(w, http.StatusOK, PushResp{Committed: committed})
}

func (s *Server) fanoutMemory(row PushRow) {
	s.sseMu.RLock()
	defer s.sseMu.RUnlock()
	for ch := range s.sseSubscribers {
		select {
		case ch <- row:
		default:
			// Drop: subscriber is slow. SSE reconnect+since= will replay.
		}
	}
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

// handleSSE serves GET /v2/sync/sse?kind=memory[&since=<cursor>].
// It first replays the memory backlog strictly after `since`, then
// subscribes to live pushes. Emits a heartbeat (":\n\n") every 15s
// to keep proxies from closing the idle connection.
//
// Wire format: each event is one SSE data frame whose body is the JSON
// encoding of a PushRow ({"key":..., "blob":<base64>}). Heartbeats are
// SSE comments (a leading ":") and the client ignores them.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind != "memory" {
		writeError(w, http.StatusBadRequest, "only kind=memory supports SSE")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe FIRST so events that arrive between the backlog read
	// and the live loop aren't lost. We tolerate duplicates between
	// backlog and live (the client's ingester is idempotent on key).
	ch := make(chan PushRow, 64)
	s.sseMu.Lock()
	s.sseSubscribers[ch] = struct{}{}
	s.sseMu.Unlock()
	defer func() {
		s.sseMu.Lock()
		delete(s.sseSubscribers, ch)
		s.sseMu.Unlock()
	}()

	// Backlog replay: ship every memory row strictly after `since`.
	// Paginates internally via List's cursor semantics.
	since := q.Get("since")
	for {
		keys, next, err := s.backend.List(r.Context(), "memory", since, 500)
		if err != nil {
			// Connection's already 200; can't change status. Just close.
			return
		}
		for _, k := range keys {
			blob, err := s.backend.Get(r.Context(), k)
			if err != nil {
				continue
			}
			if err := writeSSEEvent(w, PushRow{Key: k, Blob: blob}); err != nil {
				return
			}
			since = k
		}
		flusher.Flush()
		if next == "" || len(keys) == 0 {
			break
		}
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ":\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case row := <-ch:
			if err := writeSSEEvent(w, row); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, row PushRow) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

// --- helpers ---

// isAllowedPushKey gates POST /v2/sync/push to the three documented key
// prefixes (sec-M2). The backend's ValidateKey only rejects empty /
// leading-slash / dot-segment keys; without this prefix check, a
// compromised client token could write blobs under arbitrary prefixes
// the server stores forever (self-DoS via disk fill / blob smuggling).
// Kept aligned with validateKind on the pull/SSE side.
func isAllowedPushKey(key string) bool {
	return strings.HasPrefix(key, "sessions/") ||
		strings.HasPrefix(key, "events/") ||
		strings.HasPrefix(key, "memory/")
}

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

// writeError emits the structured JSON error envelope (Q2). The code is
// inferred from the HTTP status — call writeErrorCoded directly when
// the call site has a more specific code (e.g. the daemon's
// CodeNoUpstream 409). See internal/serve/errors.go for the envelope
// shape and the full code vocabulary.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeErrorCoded(w, status, defaultCodeForStatus(status), msg)
}

// loadOrCreateLoginChallengeKey returns the persisted HMAC key from
// serve_meta, generating it on first boot. When meta is nil (legacy
// AuthToken-only mode), returns a fresh per-process key — that mode
// has no identity stack so the cross-restart oracle (sec-M1) does not
// apply to any real registered email there.
func loadOrCreateLoginChallengeKey(meta rsql.ServeMetaStore) ([]byte, error) {
	gen := func() ([]byte, error) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		return b, nil
	}
	if meta == nil {
		return gen()
	}
	return meta.GetOrCreateBytes(context.Background(), "login_challenge_key", gen)
}
