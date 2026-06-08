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
	// BrainKeys persists the per-brain wrapped DEKs for server-at-rest
	// envelope encryption (round-12 Part A). When set alongside MasterKey,
	// brain provisioning generates+wraps a DEK and the sync handlers
	// encrypt blobs before backend.Put / decrypt after backend.Get. When
	// nil (or MasterKey unset), blobs are stored as-is (legacy behavior).
	BrainKeys rsql.BrainKeyStore
	// MasterKey is the operator's 32-byte server-at-rest master key. When
	// set (with BrainKeys), it wraps each brain's DEK (envelope
	// encryption) and gates the at-rest encrypt/decrypt path. When nil,
	// the server skips at-rest encryption entirely and stores blobs as
	// today — slice 1 keeps this optional; the client default-flip slice
	// will make it required in multi-tenant mode. Never logged.
	MasterKey []byte
	// SingleTenant relaxes the multi-tenant brain partitioning for solo
	// self-hosters (tiers 1–2). When true: /v2/sync/* accepts the legacy
	// no-user bearer, no ?brain selector is required, and the keyspace
	// stays global (no <brain_id>/ prefix). When false (the default for
	// any deployment with an identity store), every sync call must carry
	// a per-device bearer and acts in the caller's personal brain unless
	// a ?brain=<id> selector (validated for membership) overrides it.
	SingleTenant bool
}

// Server is the HTTP handler exposed at /v2/sync/* and /v2/auth/*.
type Server struct {
	mux          *http.ServeMux
	backend      rsync.Backend
	authToken    string
	serverUsers  rsql.ServerUserStore
	devices      rsql.DeviceStore
	pairings     rsql.PairingStore
	brains       rsql.BrainStore
	brainKeys    rsql.BrainKeyStore
	memberships  rsql.MembershipStore
	singleTenant bool

	// masterKey is the operator's 32-byte server-at-rest master key (round-12
	// Part A). When non-nil (with brainKeys), brain provisioning wraps a
	// per-brain DEK and the sync handlers encrypt/decrypt blobs at rest.
	// When nil, the at-rest path is skipped and blobs are stored as-is.
	// Never logged.
	masterKey []byte

	// dekCacheMu + dekCache memoize the unwrapped per-brain DEK so we
	// don't re-fetch + re-unwrap on every blob within (and across)
	// requests. The wrapped DEK lives in brain_keys; this cache only holds
	// the plaintext DEK in memory for the process lifetime. Keyed by
	// brainID.
	dekCacheMu sync.RWMutex
	dekCache   map[string][]byte

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
	// Server-at-rest (round-12 Part A): the master key, when present, must
	// be exactly 32 bytes (AES-256). A configured-but-malformed key is an
	// operator error we fail fast on rather than silently disable.
	if cfg.MasterKey != nil && len(cfg.MasterKey) != 32 {
		return nil, fmt.Errorf("serve: master key must be 32 bytes, got %d", len(cfg.MasterKey))
	}
	// Round-12 Part A slice 2 (default-flip): in MULTI-TENANT mode the
	// client default policy is server-side, so the daemon sends PLAINTEXT
	// over TLS and the server's at-rest DEK is the sole encryption layer.
	// A multi-tenant server with no master key would therefore persist
	// plaintext for every brain — so we require the key here rather than
	// silently degrading. Single-tenant solo self-hosters keep zero-
	// knowledge client sealing, so the master key stays optional there.
	if !cfg.SingleTenant && len(cfg.MasterKey) == 0 {
		return nil, errors.New("serve: multi-tenant serve requires a 32-byte master key (clients send plaintext under the default server-side policy)")
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
		brainKeys:         cfg.BrainKeys,
		memberships:       cfg.Memberships,
		singleTenant:      cfg.SingleTenant,
		masterKey:         cfg.MasterKey,
		dekCache:          map[string][]byte{},
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
	// /v2/sync/* are brain-scoped: requireBrain resolves the acting brain
	// from the ?brain selector (default: personal), validates membership,
	// and the handlers prepend/strip the brain prefix transparently.
	s.mux.HandleFunc("GET /v2/sync/whoami", s.requireBrain(s.handleWhoami))
	s.mux.HandleFunc("POST /v2/sync/push", s.requireBrain(s.handlePush))
	s.mux.HandleFunc("GET /v2/sync/pull", s.requireBrain(s.handlePull))
	s.mux.HandleFunc("GET /v2/sync/sse", s.requireBrain(s.handleSSE))
}

// multiTenant reports whether this server partitions data by brain and
// requires a per-device (user-identifying) bearer on /v2/sync/*. It is
// the inverse of single-tenant mode. The round-12 client seal-decision
// and the master-key requirement both key off this.
func (s *Server) multiTenant() bool {
	return !s.singleTenant
}

// ServeHTTP makes *Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// --- middleware ---

// /v2/sync/* auth + brain scoping is handled by requireBrain (tenancy.go).

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "resleeve-serve", "version": "v2"})
}

// WhoamiResp is the wire body for GET /v2/sync/whoami (round-12 Part A,
// slice 2). It is the daemon's seal-decision handshake: the client reads
// MultiTenant + EncryptionPolicy and computes shouldSeal =
// !MultiTenant || EncryptionPolicy == "e2e". The endpoint is auth-only —
// it returns NO plaintext blob bytes — so the daemon may probe it even
// while sync is parked pre-login.
//
// In single-tenant mode MultiTenant is false and BrainID/EncryptionPolicy
// are empty: there is no brain partitioning and the daemon always seals
// (zero-knowledge), matching the legacy solo behavior. In multi-tenant
// mode the fields describe the acting brain (the ?brain selector,
// defaulting to the caller's personal brain).
type WhoamiResp struct {
	MultiTenant      bool   `json:"multi_tenant"`
	BrainID          string `json:"brain_id"`
	EncryptionPolicy string `json:"encryption_policy"`
}

// handleWhoami answers the seal-decision handshake. It is wrapped by
// requireBrain, so by the time we get here the caller is authenticated
// and (in multi-tenant mode) the acting brain is resolved + membership-
// validated. Single-tenant mode reports {multi_tenant:false} with empty
// brain/policy.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	bc, ok := brainFromContext(r)
	if !ok {
		writeError(w, http.StatusInternalServerError, "missing brain context")
		return
	}
	resp := WhoamiResp{MultiTenant: s.multiTenant()}
	if !s.multiTenant() {
		// Single-tenant: no brain, no per-brain policy. shouldSeal is
		// forced true client-side off MultiTenant=false.
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.BrainID = bc.brainID
	// Resolve the acting brain's encryption policy. A brain row always
	// carries a policy (defaults to server-side at the storage layer), so
	// a lookup miss is an internal inconsistency, not a client error.
	if s.brains != nil && bc.brainID != "" {
		b, err := s.brains.Get(r.Context(), bc.brainID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "lookup brain policy: "+err.Error())
			return
		}
		resp.EncryptionPolicy = string(b.EncryptionPolicy)
	}
	if resp.EncryptionPolicy == "" {
		resp.EncryptionPolicy = string(rsql.EncryptionPolicyServerSide)
	}
	writeJSON(w, http.StatusOK, resp)
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
	bc, ok := brainFromContext(r)
	if !ok {
		writeError(w, http.StatusInternalServerError, "missing brain context")
		return
	}
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
		// sec-M2: gate the (brain-agnostic) key prefix so a misbehaving or
		// compromised client can't write blobs under arbitrary prefixes
		// (disk fill / blob smuggling). The three allowed prefixes mirror
		// the wire kinds validated by validateKind on the pull/SSE side.
		// The client supplies a brain-agnostic key; the server prepends
		// the acting brain so a client can never forge a cross-brain
		// prefix (the brain comes from the authenticated membership, not
		// the request body).
		if !isAllowedPushKey(row.Key) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("rejected key %q: must start with sessions/, events/, or memory/", row.Key))
			return
		}
		storedKey := scopeKey(bc.brainID, row.Key)
		// Server-at-rest (round-12 Part A): encrypt the blob with the brain
		// DEK before persisting. No-op (returns row.Blob) when no master
		// key / brain DEK is configured. The SSE fan-out below still ships
		// the PLAINTEXT row.Blob, matching what pull returns to clients.
		stored, err := s.encryptForBrain(r.Context(), bc.brainID, row.Blob)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("encrypt %q: %v", storedKey, err))
			return
		}
		if err := s.backend.Put(r.Context(), storedKey, stored); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("put %q: %v", storedKey, err))
			return
		}
		// Ack the CLIENT's (brain-agnostic) key so the daemon's outbox
		// ack matches what it sent.
		committed = append(committed, row.Key)
		// Fast-tier fan-out: memory rows trigger live delivery to SSE
		// subscribers. We fan out the STORED (brain-prefixed) key so each
		// subscriber can filter to its own brain. Backend.Put has already
		// persisted; SSE is just the low-latency notification path. Slow
		// subscribers (full buffer) silently drop — they catch up on
		// reconnect via the backlog replay window.
		if strings.HasPrefix(row.Key, "memory/") {
			s.fanoutMemory(PushRow{Key: storedKey, Blob: row.Blob})
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
	bc, ok := brainFromContext(r)
	if !ok {
		writeError(w, http.StatusInternalServerError, "missing brain context")
		return
	}
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
	// The client sends a brain-agnostic cursor; re-scope it to the stored
	// keyspace so List walks only this brain's keys.
	since := scopeCursor(bc.brainID, q.Get("since"))
	limit := parseLimit(q.Get("limit"), 100)
	prefix := scopePrefix(bc.brainID, kind)

	keys, next, err := s.backend.List(r.Context(), prefix, since, limit)
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
		// Server-at-rest (round-12 Part A): decrypt with the brain DEK
		// before returning. No-op when no master key / brain DEK.
		plain, err := s.decryptForBrain(r.Context(), bc.brainID, blob)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("decrypt %q: %v", k, err))
			return
		}
		// Strip the brain prefix so the daemon sees brain-agnostic keys
		// and its ingest + cursor logic is unchanged across the flip.
		rows = append(rows, PushRow{Key: unscopeKey(bc.brainID, k), Blob: plain})
	}
	writeJSON(w, http.StatusOK, PullResp{Rows: rows, NextCursor: unscopeKey(bc.brainID, next)})
}

// scopeCursor re-prefixes a client-supplied (brain-agnostic) pagination
// cursor so backend.List walks only the acting brain's keyspace. An empty
// cursor (first page) stays empty. Single-tenant mode is a no-op.
func scopeCursor(brainID, cursor string) string {
	if brainID == "" || cursor == "" {
		return cursor
	}
	return brainID + "/" + cursor
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
	bc, ok := brainFromContext(r)
	if !ok {
		writeError(w, http.StatusInternalServerError, "missing brain context")
		return
	}
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

	// Backlog replay: ship every memory row strictly after `since`, scoped
	// to the acting brain. Paginates internally via List's cursor semantics.
	memPrefix := scopePrefix(bc.brainID, "memory")
	since := scopeCursor(bc.brainID, q.Get("since"))
	for {
		keys, next, err := s.backend.List(r.Context(), memPrefix, since, 500)
		if err != nil {
			// Connection's already 200; can't change status. Just close.
			return
		}
		for _, k := range keys {
			blob, err := s.backend.Get(r.Context(), k)
			if err != nil {
				continue
			}
			// Server-at-rest (round-12 Part A): decrypt the backlog row with
			// the brain DEK. The live fan-out path below already carries the
			// plaintext blob (from push), so only this stored-read branch
			// decrypts. No-op when no master key / brain DEK.
			plain, err := s.decryptForBrain(r.Context(), bc.brainID, blob)
			if err != nil {
				continue
			}
			if err := writeSSEEvent(w, PushRow{Key: unscopeKey(bc.brainID, k), Blob: plain}); err != nil {
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
			// Live fan-out rows carry the STORED (brain-prefixed) key.
			// Deliver only those under the acting brain, and strip the
			// prefix so the client sees brain-agnostic keys.
			if bc.brainID != "" && !strings.HasPrefix(row.Key, bc.brainID+"/") {
				continue
			}
			if err := writeSSEEvent(w, PushRow{Key: unscopeKey(bc.brainID, row.Key), Blob: row.Blob}); err != nil {
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
