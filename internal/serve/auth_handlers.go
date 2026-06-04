package serve

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/auth"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// --- Argon2id params policy (sec-H1) ---
//
// The client picks its own Argon2id cost knobs and ships them in
// RegisterReq.Params. We trust the wire shape but NOT the cost: a
// malicious client could pick (memory_kib=8, time_iters=1) to produce a
// verifier hash that's offline-crackable in seconds if the DB ever leaks.
// Per G's security review (H1), we therefore enforce a server-side floor
// BEFORE persisting the user row. We also enforce a CEILING so that a
// malicious caller can't single-shot DoS the server by demanding 64 GiB
// of memory or 1e6 iterations during register (the server itself doesn't
// run Argon2 here, but accepting absurd params poisons the stored row
// and impacts the legitimate client on every subsequent login).
//
// Floor matches round-2/10's baseline (OWASP-leaning interactive auth).
// Ceiling is generous — anything above it is presumptive abuse.
const (
	argon2MinMemoryKiB   uint32 = 64 * 1024 // 64 MiB — OWASP interactive floor
	argon2MaxMemoryKiB   uint32 = 4 * 1024 * 1024 // 4 GiB — DoS ceiling
	argon2MinTimeIters   uint32 = 2
	argon2MaxTimeIters   uint32 = 32
	argon2MinParallelism uint8  = 1
	argon2RequiredSaltLen   = 16 // 128 bits — matches auth.NewSalt()
	argon2RequiredOutputLen = 32 // 256 bits — matches auth.keyLen (AES-256-GCM)
)

// Auth-handler routes (added in punch-list #3). Wire format mirrors the
// docs/design/round-4/02-cross-machine-sync.md §"Identity" flow:
//
//   POST /v2/auth/register      → create server_users + first device
//   POST /v2/auth/login         → verify password, mint a device token
//   POST /v2/auth/logout        → revoke the presented device token
//   POST /v2/auth/pair/publish  → create a pairing_codes row, return code
//   POST /v2/auth/pair/claim    → verify code, mint device token,
//                                 return wrapped KEK for unwrap on the
//                                 accepter's local machine
//
// The server never sees the plaintext password or KEK. Argon2id runs on
// the client; the server stores only the (verifier, wrapped_kek) tuples.
// Login returns the wrapped KEK so the client can unwrap locally — the
// network never carries the KEK in clear.

// --- wire types ---

// RegisterReq is the body of POST /v2/auth/register.
// The client has already locally:
//   - derived password_verifier (Argon2id over password+salt)
//   - generated a fresh KEK
//   - wrapped the KEK under the password and under a recovery key
//   - generated the recovery key (returned to the user once)
//
// The server's job is solely to persist the resulting tuple keyed by
// email, and mint the device token for the registering machine.
type RegisterReq struct {
	Email    string         `json:"email"`
	Params   Argon2idParams `json:"params"`
	Password PasswordEnv    `json:"password"`
	Recovery PasswordEnv    `json:"recovery"`
	Device   DeviceMetadata `json:"device"`
}

// Argon2idParams mirrors auth.Argon2idParams over the wire.
type Argon2idParams struct {
	MemoryKiB   uint32 `json:"memory_kib"`
	TimeIters   uint32 `json:"time_iters"`
	Parallelism uint8  `json:"parallelism"`
}

// PasswordEnv carries verifier + KEK wrap material for one "factor"
// (password or recovery key). Salts are independent so the server-stored
// verifier cannot help an attacker unwrap the KEK.
type PasswordEnv struct {
	VerifierSalt []byte `json:"verifier_salt"`
	VerifierHash []byte `json:"verifier_hash"`
	KEKSalt      []byte `json:"kek_salt"`
	KEKNonce     []byte `json:"kek_nonce"`
	KEKCT        []byte `json:"kek_ct"`
}

// DeviceMetadata is the optional human-readable name the device wants
// to register under (e.g. "matt's laptop"). Purely informational.
type DeviceMetadata struct {
	Name string `json:"name"`
}

// RegisterResp returns the assigned user_id + the first device token.
type RegisterResp struct {
	UserID      string `json:"user_id"`
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
}

// LoginReq is POST /v2/auth/login: the client presents its verifier
// hash (NOT the password) computed against the salt it was given by
// /v2/auth/login-challenge. v1 simplification: we ship the email +
// password-derived verifier hash in one round trip, because the client
// needs the WrappedKEK to decrypt local content anyway and we'd be
// returning it on success regardless.
//
// ExpiresAtHintNS is the optional client-requested device-token TTL in
// nanoseconds (sec-M5). Non-zero values request a server-side expiry on
// the minted device row so the bearer is rejected after that delta even
// if the client never calls /v2/auth/logout. Used by `resleeve pair
// invite` to mint a short-lived "pair-invite-ephemeral" token (60s).
// The server clamps to [0, deviceExpiresAtHintMax]; zero (the default
// for ordinary `resleeve login`) means no expiry — matches the v1
// behavior on regular paired devices.
type LoginReq struct {
	Email           string         `json:"email"`
	VerifierHash    []byte         `json:"verifier_hash"`
	Device          DeviceMetadata `json:"device"`
	ExpiresAtHintNS int64          `json:"expires_at_hint_ns,omitempty"`
}

// deviceExpiresAtHintMax caps the client-requested TTL on
// /v2/auth/login (sec-M5). 10 minutes is overshoot for the documented
// pair-invite use case (60s) but leaves headroom for slow operators
// without permitting an effectively-unbounded TTL via a malicious or
// buggy client. Hints above the cap are clamped, not rejected, so a
// future call-site requesting a slightly larger value still gets a
// finite expiry rather than an unbounded bearer.
const deviceExpiresAtHintMax = 10 * time.Minute

// LoginResp contains everything the client needs to unwrap its KEK
// locally: the params, the verifier salt (echoed so the client can
// double-check), and the WrappedKEK. The KEK itself is unwrapped on
// the client and never traverses the network.
type LoginResp struct {
	UserID      string         `json:"user_id"`
	DeviceID    string         `json:"device_id"`
	DeviceToken string         `json:"device_token"`
	Params      Argon2idParams `json:"params"`
	WrappedKEK  WrappedKEKEnv  `json:"wrapped_kek"`
}

// WrappedKEKEnv is the over-the-wire form of an auth.WrappedKEK. We
// don't reuse PasswordEnv because the verifier salt/hash are absent
// here — the server already verified on the way in.
type WrappedKEKEnv struct {
	Salt  []byte `json:"salt"`
	Nonce []byte `json:"nonce"`
	CT    []byte `json:"ct"`
}

// LoginChallengeReq is POST /v2/auth/login-challenge: the client says
// who it is and the server returns the verifier salt + Argon2 params
// so the client can derive the same VerifierHash to ship in LoginReq.
type LoginChallengeReq struct {
	Email string `json:"email"`
}

// LoginChallengeResp returns the per-account Argon2 cost knobs +
// verifier salt. Public per-account info: leak does not compromise the
// KEK because the KEK uses a different salt (round-2/10 design).
type LoginChallengeResp struct {
	Params       Argon2idParams `json:"params"`
	VerifierSalt []byte         `json:"verifier_salt"`
}

// --- handler registration ---

func (s *Server) registerAuthRoutes() {
	// Auth endpoints are NOT bearer-gated (the act of authenticating
	// can't require a prior token). They have their own validation.
	s.mux.HandleFunc("POST /v2/auth/register", s.handleRegister)
	s.mux.HandleFunc("POST /v2/auth/login-challenge", s.handleLoginChallenge)
	s.mux.HandleFunc("POST /v2/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /v2/auth/logout", s.handleLogout)
	s.mux.HandleFunc("POST /v2/auth/pair/publish", s.requireDevice(s.handlePairPublish))
	s.mux.HandleFunc("POST /v2/auth/pair/claim", s.handlePairClaim)
}

// --- register ---

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.serverUsers == nil || s.devices == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store not configured")
		return
	}
	var req RegisterReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("decode: %v", err))
		return
	}
	if err := validateRegister(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if _, err := s.serverUsers.GetByEmail(r.Context(), email); err == nil {
		writeError(w, http.StatusConflict, "email already registered")
		return
	} else if !errors.Is(err, rsql.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now().UTC()
	u := &rsql.ServerUser{
		ID:        newID(16),
		Email:     email,
		CreatedAt: now,
		UpdatedAt: now,
		Params:    paramsFromWire(req.Params),
		PasswordVerifier: auth.Verifier{
			Salt: req.Password.VerifierSalt,
			Hash: req.Password.VerifierHash,
		},
		PasswordKEK: auth.WrappedKEK{
			Salt:       req.Password.KEKSalt,
			Nonce:      req.Password.KEKNonce,
			Ciphertext: req.Password.KEKCT,
			Params:     paramsFromWire(req.Params),
		},
		RecoveryVerifier: auth.Verifier{
			Salt: req.Recovery.VerifierSalt,
			Hash: req.Recovery.VerifierHash,
		},
		RecoveryKEK: auth.WrappedKEK{
			Salt:       req.Recovery.KEKSalt,
			Nonce:      req.Recovery.KEKNonce,
			Ciphertext: req.Recovery.KEKCT,
			Params:     paramsFromWire(req.Params),
		},
	}
	if err := s.serverUsers.Create(r.Context(), u); err != nil {
		writeError(w, http.StatusInternalServerError, "create user: "+err.Error())
		return
	}

	dev, err := s.mintDevice(r.Context(), u.ID, req.Device.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint device: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, RegisterResp{
		UserID:      u.ID,
		DeviceID:    dev.ID,
		DeviceToken: dev.DeviceToken,
	})
}

// --- login challenge ---

func (s *Server) handleLoginChallenge(w http.ResponseWriter, r *http.Request) {
	if s.serverUsers == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store not configured")
		return
	}
	var req LoginChallengeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	// Info-leak hardening: do NOT 404 on unknown emails — that turns the
	// challenge endpoint into a registration oracle. Instead, return a
	// constant-shape challenge with default Argon2id params and a
	// deterministic-but-secret synthetic salt derived from a per-process
	// server secret. The follow-up /v2/auth/login still 401s on unknown
	// emails (verifier mismatch); the client UX is identical, but a
	// passive observer cannot distinguish a real from a decoy challenge.
	u, err := s.serverUsers.GetByEmail(r.Context(), email)
	if err != nil {
		if !errors.Is(err, rsql.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, LoginChallengeResp{
			Params:       paramsToWire(auth.DefaultArgon2idParams()),
			VerifierSalt: s.syntheticChallengeSalt(email),
		})
		return
	}
	writeJSON(w, http.StatusOK, LoginChallengeResp{
		Params:       paramsToWire(u.Params),
		VerifierSalt: u.PasswordVerifier.Salt,
	})
}

// runLoginTimingDecoy performs a dummy subtle.ConstantTimeCompare on
// the unknown-email branch of /v2/auth/login (sec-M6) so the timing
// shape of "no such email" matches "wrong password". The decoy is a
// per-process random 32-byte value; the compare deterministically
// returns 0 (the inputs are independent random). The return value is
// intentionally consumed via the side effect alone — we don't care
// what it is, we just want the compare to happen so the caller can't
// time-distinguish the early-return path from the verifier-check path.
//
// Truncated to len(hash) up to the decoy length so the constant-time
// compare runs over the same byte count as the real path would. If the
// client ships an absurdly long hash, we cap at the decoy length —
// the cost gap between "decoy-len compare" and "32-byte real compare"
// is far below DB-jitter and the gross "no compare at all" tell is
// what matters here.
func (s *Server) runLoginTimingDecoy(verifierHash []byte) {
	n := len(verifierHash)
	if n > len(s.loginTimingDecoy) {
		n = len(s.loginTimingDecoy)
	}
	if n == 0 {
		// Run anyway over the decoy alone so an empty hash still hits
		// the constant-time path; client-side empty-hash is a malformed
		// request shape, not a real timing channel, but be defensive.
		_ = subtle.ConstantTimeCompare(s.loginTimingDecoy, s.loginTimingDecoy)
		return
	}
	_ = subtle.ConstantTimeCompare(verifierHash[:n], s.loginTimingDecoy[:n])
}

// syntheticChallengeSalt derives a stable-but-secret 16-byte salt for an
// unknown email. Determinism (same email → same salt within a process
// lifetime) means a repeat probe doesn't reveal "this email is new"; the
// HMAC key keeps the mapping unguessable so an attacker can't precompute
// salts to distinguish. Length matches auth.NewSalt() (128 bits) so the
// wire shape is identical to a real PasswordVerifier.Salt.
func (s *Server) syntheticChallengeSalt(email string) []byte {
	mac := hmac.New(sha256.New, s.loginChallengeKey)
	_, _ = mac.Write([]byte("login-challenge-salt-v1\x00"))
	_, _ = mac.Write([]byte(email))
	sum := mac.Sum(nil)
	return sum[:16]
}

// --- login ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.serverUsers == nil || s.devices == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store not configured")
		return
	}
	var req LoginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	u, err := s.serverUsers.GetByEmail(r.Context(), strings.ToLower(strings.TrimSpace(req.Email)))
	if err != nil {
		// sec-M6: on the unknown-email branch, run a dummy
		// ConstantTimeCompare against a per-process random decoy before
		// returning 401 so the timing shape matches the known-email
		// wrong-password branch (which always runs the compare against
		// PasswordVerifier.Hash). The decoy is random + non-secret, so
		// the compare deterministically returns 0; this is purely about
		// closing the early-return distinguisher between "no such email"
		// and "wrong password". DB-round-trip time still differs in
		// principle, but the compare-vs-no-compare gap is the easy tell
		// we're plugging.
		s.runLoginTimingDecoy(req.VerifierHash)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	// Constant-time verifier compare. The client computed
	// Argon2id(password, salt) using the salt from /login-challenge;
	// we compare against the stored hash. NOT a timing-safe replacement
	// for the full ZK round-trip, but adequate for v1 where the wire
	// is TLS and the server already holds the verifier.
	if subtle.ConstantTimeCompare(req.VerifierHash, u.PasswordVerifier.Hash) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	// sec-M5: honor the client's ExpiresAtHintNS for short-lived
	// devices (pair-invite ephemeral). Clamped to deviceExpiresAtHintMax;
	// zero (default) preserves the pre-M5 no-expiry behavior for ordinary
	// `resleeve login`.
	ttl := time.Duration(req.ExpiresAtHintNS)
	if ttl < 0 {
		ttl = 0
	}
	if ttl > deviceExpiresAtHintMax {
		ttl = deviceExpiresAtHintMax
	}
	dev, err := s.mintDeviceWithTTL(r.Context(), u.ID, req.Device.Name, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint device: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, LoginResp{
		UserID:      u.ID,
		DeviceID:    dev.ID,
		DeviceToken: dev.DeviceToken,
		Params:      paramsToWire(u.Params),
		WrappedKEK: WrappedKEKEnv{
			Salt:  u.PasswordKEK.Salt,
			Nonce: u.PasswordKEK.Nonce,
			CT:    u.PasswordKEK.Ciphertext,
		},
	})
}

// --- logout ---

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store not configured")
		return
	}
	dev, ok := s.deviceFromBearer(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	// sec-H3: logout deletes a device row by ID. The synthetic legacy
	// device has ID="legacy" and was never persisted; calling Delete on
	// it would silently no-op (or, worse, target a real row if someone
	// later registered a device with that name). Reject the legacy
	// fallback explicitly so logout only operates on real per-device
	// tokens.
	if !s.requireUserDevice(w, r, dev) {
		return
	}
	if err := s.devices.Delete(r.Context(), dev.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "delete device: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// --- pairing ---

// PairPublishReq is POST /v2/auth/pair/publish — the inviter, already
// authenticated as a device, ships:
//   - an optional client-allocated code_id (so the verifier salt can
//     be deterministic over it; empty = server allocates)
//   - the verifier salt+hash for the random pair code
//   - the KEK wrapped under a key derived from the same pair code
//
// The server sets the 5-minute TTL.
type PairPublishReq struct {
	CodeID   string         `json:"code_id,omitempty"`
	Params   Argon2idParams `json:"params"`
	Verifier VerifierEnv    `json:"verifier"`
	Wrapped  WrappedKEKEnv  `json:"wrapped"`
	TTL      time.Duration  `json:"ttl_ns,omitempty"`
}

// VerifierEnv is the wire form of auth.Verifier.
type VerifierEnv struct {
	Salt []byte `json:"salt"`
	Hash []byte `json:"hash"`
}

// PairPublishResp returns the public CodeID the inviter prints. The
// pair code itself was generated client-side and is NEVER sent.
type PairPublishResp struct {
	CodeID    string    `json:"code_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handlePairPublish(w http.ResponseWriter, r *http.Request, dev *rsql.Device) {
	if s.pairings == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store not configured")
		return
	}
	// sec-H3: pair/publish writes a pairing_codes row scoped by user_id.
	// The legacy single-bearer fallback has no user, so it must NOT be
	// able to mint pairing rows under an empty user.
	if !s.requireUserDevice(w, r, dev) {
		return
	}
	var req PairPublishReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	ttl := req.TTL
	if ttl <= 0 || ttl > 10*time.Minute {
		ttl = 5 * time.Minute
	}
	now := time.Now().UTC()
	codeID := strings.TrimSpace(req.CodeID)
	if codeID == "" {
		codeID = newID(8)
	}
	pc := &rsql.PairingCode{
		CodeID:    codeID,
		UserID:    dev.UserID,
		Params:    paramsFromWire(req.Params),
		Verifier:  auth.Verifier{Salt: req.Verifier.Salt, Hash: req.Verifier.Hash},
		WrappedK:  auth.WrappedKEK{Salt: req.Wrapped.Salt, Nonce: req.Wrapped.Nonce, Ciphertext: req.Wrapped.CT, Params: paramsFromWire(req.Params)},
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	if err := s.pairings.Create(r.Context(), pc); err != nil {
		writeError(w, http.StatusInternalServerError, "create code: "+err.Error())
		return
	}
	// Opportunistic sweep — cheap.
	_ = s.pairings.SweepExpired(r.Context(), now)
	writeJSON(w, http.StatusCreated, PairPublishResp{CodeID: pc.CodeID, ExpiresAt: pc.ExpiresAt})
}

// PairClaimReq is POST /v2/auth/pair/claim.
type PairClaimReq struct {
	CodeID       string         `json:"code_id"`
	VerifierHash []byte         `json:"verifier_hash"`
	Device       DeviceMetadata `json:"device"`
}

// PairClaimResp returns the wrapped KEK (still encrypted under the pair
// code; the accepter unwraps locally after typing the code) plus a
// fresh device token. The KEK plaintext never leaves the inviter.
type PairClaimResp struct {
	UserID      string         `json:"user_id"`
	DeviceID    string         `json:"device_id"`
	DeviceToken string         `json:"device_token"`
	Params      Argon2idParams `json:"params"`
	Wrapped     WrappedKEKEnv  `json:"wrapped"`
}

func (s *Server) handlePairClaim(w http.ResponseWriter, r *http.Request) {
	if s.pairings == nil || s.devices == nil {
		writeError(w, http.StatusServiceUnavailable, "identity store not configured")
		return
	}
	var req PairClaimReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	pc, err := s.pairings.Get(r.Context(), req.CodeID)
	if err != nil {
		// Either the code never existed, was swept on expiry, or was
		// locked out via pairClaimMaxAttempts. Same wire response for
		// all three so a brute-forcer cannot distinguish lockout from
		// normal TTL expiry (sec-H2).
		writePairClaimGone(w)
		return
	}
	now := time.Now().UTC()
	if pc.ClaimedAt != nil {
		// Replay after a successful claim — distinct from lockout/TTL
		// expiry: the legitimate accepter already finished, so 410
		// "already used" is still the right signal to the operator.
		writeError(w, http.StatusGone, "code already used or expired")
		return
	}
	if pc.ExpiresAt.Before(now) {
		// Lazy TTL sweep: delete on detection and clear any counter
		// state so this path is byte-identical to the lockout path.
		s.pairClaimLockout(r.Context(), pc.CodeID)
		writePairClaimGone(w)
		return
	}
	if subtle.ConstantTimeCompare(req.VerifierHash, pc.Verifier.Hash) != 1 {
		// Increment AFTER the comparison so a single failure costs the
		// attacker one of their budgeted attempts. At the threshold we
		// hard-delete the code row + clear in-memory state and emit the
		// lockout response (same shape as TTL expiry).
		if s.pairClaimRecordFailure(pc.CodeID) >= pairClaimMaxAttempts {
			s.pairClaimLockout(r.Context(), pc.CodeID)
			writePairClaimGone(w)
			return
		}
		writeError(w, http.StatusUnauthorized, "code verification failed")
		return
	}
	// Atomic claim before minting the device so a concurrent claim loses.
	if err := s.pairings.Claim(r.Context(), pc.CodeID, now); err != nil {
		writeError(w, http.StatusGone, "code already used")
		return
	}
	// Success: drop any accumulated counter so subsequent unrelated
	// probes (which can't succeed anyway, since ClaimedAt != nil now)
	// don't carry over state. Belt-and-braces — no information leak
	// hinges on this, but leaving leftover state in the map is sloppy.
	s.pairClaimClearAttempts(pc.CodeID)
	dev, err := s.mintDevice(r.Context(), pc.UserID, req.Device.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint device: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, PairClaimResp{
		UserID:      pc.UserID,
		DeviceID:    dev.ID,
		DeviceToken: dev.DeviceToken,
		Params:      paramsToWire(pc.Params),
		Wrapped: WrappedKEKEnv{
			Salt:  pc.WrappedK.Salt,
			Nonce: pc.WrappedK.Nonce,
			CT:    pc.WrappedK.Ciphertext,
		},
	})
}

// writePairClaimGone is the unified "code not available" response used
// by handlePairClaim for both TTL-swept codes and codes locked out via
// pairClaimMaxAttempts. Returning identical status + body for both is
// the indistinguishability guarantee that justifies sec-H2's fix
// strategy: an attacker probing a leaked code_id cannot tell whether
// the code expired naturally or whether their brute force tripped the
// lockout.
func writePairClaimGone(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "code not found or expired")
}

// pairClaimRecordFailure increments the per-code failure counter and
// returns the new count. Caller checks the threshold and triggers the
// lockout if it's reached.
func (s *Server) pairClaimRecordFailure(codeID string) int {
	s.pairAttemptsMu.Lock()
	defer s.pairAttemptsMu.Unlock()
	s.pairAttempts[codeID]++
	return s.pairAttempts[codeID]
}

// pairClaimClearAttempts drops the in-memory counter for a code. Called
// on successful claim and on lockout-delete; both transition the code
// to a terminal state where the counter is no longer needed.
func (s *Server) pairClaimClearAttempts(codeID string) {
	s.pairAttemptsMu.Lock()
	defer s.pairAttemptsMu.Unlock()
	delete(s.pairAttempts, codeID)
}

// pairClaimLockout hard-deletes a pair code and clears its in-memory
// counter. Errors from Delete are swallowed by design — the caller is
// about to emit a "not found" response, and the next claim will Get
// ErrNotFound anyway (either because Delete succeeded or because the
// row was already gone). Idempotent and safe to call on rows that
// don't exist.
func (s *Server) pairClaimLockout(ctx context.Context, codeID string) {
	_ = s.pairings.Delete(ctx, codeID)
	s.pairClaimClearAttempts(codeID)
}

// --- middleware: per-device bearer ---

// requireDevice is the per-device auth middleware. Tokens are looked up
// in the devices table; the device is passed through to the handler so
// it can scope writes by user_id without an extra DB round trip.
//
// Backward-compat: when a legacy AuthToken is configured and matches,
// we fall back to a synthetic "legacy" device row attached to NO user,
// so existing single-bearer deployments keep working through the
// migration. Sync routes still accept this; identity routes reject it
// (they explicitly check for dev.UserID != "").
func (s *Server) requireDevice(next func(http.ResponseWriter, *http.Request, *rsql.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dev, ok := s.deviceFromBearer(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next(w, r, dev)
	}
}

func (s *Server) deviceFromBearer(r *http.Request) (*rsql.Device, bool) {
	got := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(got, prefix) {
		return nil, false
	}
	token := strings.TrimPrefix(got, prefix)
	// 1) Per-device lookup (post-migration path).
	if s.devices != nil {
		if dev, err := s.devices.GetByToken(r.Context(), token); err == nil {
			// sec-M5: enforce the server-side TTL. Expired tokens are
			// treated as if the row didn't exist so the wire response
			// (401 from the caller) is indistinguishable from a never-
			// existed bearer. We also opportunistically delete the
			// expired row — best-effort, ignore errors. The bearer was
			// minted with a short TTL precisely so a missed best-effort
			// revoke on the client side doesn't leave it usable.
			if dev.ExpiresAt != nil && !time.Now().UTC().Before(*dev.ExpiresAt) {
				_ = s.devices.Delete(r.Context(), dev.ID)
				return nil, false
			}
			_ = s.devices.TouchLastSeen(r.Context(), dev.ID)
			return dev, true
		}
	}
	// 2) Legacy single-bearer fallback (pre-migration deployments).
	if s.authToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) == 1 {
		return &rsql.Device{ID: "legacy", UserID: "", Name: "legacy-bearer"}, true
	}
	return nil, false
}

// requireUserDevice gates endpoints that require a real user identity
// (i.e. anything that writes rows scoped by user_id). The legacy
// single-bearer fallback in deviceFromBearer returns a synthetic device
// with UserID="" — that's fine for content-blind /v2/sync/* routes, but
// NOT fine for identity routes like pair/publish (sec-H3): otherwise a
// legacy bearer can mint pairing rows under an empty user.
//
// Returns true when the caller may proceed. Writes 401 + logs a warning
// when the bearer is a legacy single-token (no user). The warning is
// the operator's nudge to migrate off the legacy path.
func (s *Server) requireUserDevice(w http.ResponseWriter, r *http.Request, dev *rsql.Device) bool {
	if dev != nil && dev.UserID != "" {
		return true
	}
	log.Printf("serve: legacy bearer used for %s %s, which requires a real user identity; consider running `resleeve register` + per-device tokens",
		r.Method, r.URL.Path)
	writeError(w, http.StatusUnauthorized, "endpoint requires a per-device token; legacy bearer cannot identify a user")
	return false
}

// --- helpers ---

func (s *Server) mintDevice(ctx context.Context, userID, name string) (*rsql.Device, error) {
	return s.mintDeviceWithTTL(ctx, userID, name, 0)
}

// mintDeviceWithTTL is mintDevice with a server-side expiry on the
// device row (sec-M5). When ttl > 0, the device's ExpiresAt is set to
// now+ttl, and deviceFromBearer rejects the token after that time.
// ttl <= 0 means no expiry (matches mintDevice's behavior for ordinary
// register/login). Used by handleLogin when the client requests a
// short-lived ephemeral device (e.g. `resleeve pair invite`).
func (s *Server) mintDeviceWithTTL(ctx context.Context, userID, name string, ttl time.Duration) (*rsql.Device, error) {
	tok, err := newDeviceToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	dev := &rsql.Device{
		ID:          newID(16),
		UserID:      userID,
		Name:        name,
		DeviceToken: tok,
		CreatedAt:   now,
		LastSeenAt:  now,
	}
	if ttl > 0 {
		exp := now.Add(ttl)
		dev.ExpiresAt = &exp
	}
	if err := s.devices.Create(ctx, dev); err != nil {
		return nil, err
	}
	return dev, nil
}

func newDeviceToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "dev_" + hex.EncodeToString(b), nil
}

func newID(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func paramsFromWire(p Argon2idParams) auth.Argon2idParams {
	return auth.Argon2idParams{MemoryKiB: p.MemoryKiB, TimeIters: p.TimeIters, Parallelism: p.Parallelism}
}

func paramsToWire(p auth.Argon2idParams) Argon2idParams {
	return Argon2idParams{MemoryKiB: p.MemoryKiB, TimeIters: p.TimeIters, Parallelism: p.Parallelism}
}

func validateRegister(req *RegisterReq) error {
	if strings.TrimSpace(req.Email) == "" || !strings.Contains(req.Email, "@") {
		return errors.New("invalid email")
	}
	if err := validateArgon2Params(req.Params); err != nil {
		return err
	}
	if len(req.Password.VerifierSalt) != argon2RequiredSaltLen {
		return fmt.Errorf("password verifier salt must be %d bytes, got %d", argon2RequiredSaltLen, len(req.Password.VerifierSalt))
	}
	if len(req.Recovery.VerifierSalt) != argon2RequiredSaltLen {
		return fmt.Errorf("recovery verifier salt must be %d bytes, got %d", argon2RequiredSaltLen, len(req.Recovery.VerifierSalt))
	}
	if len(req.Password.VerifierHash) != argon2RequiredOutputLen {
		return fmt.Errorf("password verifier hash must be %d bytes, got %d", argon2RequiredOutputLen, len(req.Password.VerifierHash))
	}
	if len(req.Recovery.VerifierHash) != argon2RequiredOutputLen {
		return fmt.Errorf("recovery verifier hash must be %d bytes, got %d", argon2RequiredOutputLen, len(req.Recovery.VerifierHash))
	}
	if len(req.Password.KEKCT) == 0 {
		return errors.New("missing password envelope")
	}
	if len(req.Recovery.KEKCT) == 0 {
		return errors.New("missing recovery envelope")
	}
	return nil
}

// validateArgon2Params enforces the server-side floor + ceiling on the
// client-supplied Argon2id cost knobs (sec-H1). The floor blocks a
// malicious client from registering with verifier-cracking-cheap params;
// the ceiling blocks a single-shot DoS via absurd params that would
// punish every legitimate login thereafter.
func validateArgon2Params(p Argon2idParams) error {
	if p.MemoryKiB == 0 || p.TimeIters == 0 || p.Parallelism == 0 {
		return errors.New("missing argon2 params")
	}
	if p.MemoryKiB < argon2MinMemoryKiB {
		return fmt.Errorf("argon2 memory_kib %d below floor %d", p.MemoryKiB, argon2MinMemoryKiB)
	}
	if p.MemoryKiB > argon2MaxMemoryKiB {
		return fmt.Errorf("argon2 memory_kib %d above ceiling %d", p.MemoryKiB, argon2MaxMemoryKiB)
	}
	if p.TimeIters < argon2MinTimeIters {
		return fmt.Errorf("argon2 time_iters %d below floor %d", p.TimeIters, argon2MinTimeIters)
	}
	if p.TimeIters > argon2MaxTimeIters {
		return fmt.Errorf("argon2 time_iters %d above ceiling %d", p.TimeIters, argon2MaxTimeIters)
	}
	if p.Parallelism < argon2MinParallelism {
		return fmt.Errorf("argon2 parallelism %d below floor %d", p.Parallelism, argon2MinParallelism)
	}
	return nil
}
