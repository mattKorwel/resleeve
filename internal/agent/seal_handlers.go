package agent

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/mattkorwel/resleeve/internal/auth"
)

// /v1/seal/unlock and /v1/seal/lock are the runtime-mutable Sealer
// surface. `resleeve login` derives the KEK locally (from master
// password + Argon2id) and POSTs it to /v1/seal/unlock; the daemon
// installs a fresh AES-GCM Sealer backed by that key and the parked
// drain/pull/sse loops resume. `resleeve logout` POSTs to /v1/seal/lock
// which clears the Sealer, parking sync again.
//
// IMPORTANT — security boundary: these routes accept a raw KEK in the
// request body. They MUST only be exposed on loopback. The check below
// (requireLoopback) enforces this in addition to the daemon's default
// 127.0.0.1 bind: a config drift that bound to 0.0.0.0 would still be
// rejected at the route level. The server-side /v2/auth/* equivalents
// in `resleeve serve` deliberately do NOT have an unlock route — the
// KEK never traverses the network there either.

// registerSealRoutes wires the runtime sealer endpoints. Called from
// daemon.go's registerRoutes (kept here so the security comment stays
// next to the implementation).
func (d *Daemon) registerSealRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/seal/unlock", d.requireBearer(d.requireLoopback(d.handleSealUnlock)))
	mux.HandleFunc("/v1/seal/lock", d.requireBearer(d.requireLoopback(d.handleSealLock)))
	mux.HandleFunc("/v1/seal/status", d.requireBearer(d.handleSealStatus))
}

// requireLoopback rejects any request whose peer is not the loopback
// interface. Combined with the default `--addr 127.0.0.1:0` bind this
// is belt-and-suspenders, but the cost is one IP parse per request.
func (d *Daemon) requireLoopback(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			writeErrorStatus(w, http.StatusBadRequest, "bad remote addr")
			return
		}
		ip := net.ParseIP(strings.TrimSpace(host))
		if ip == nil || !ip.IsLoopback() {
			writeErrorStatus(w, http.StatusForbidden, "loopback only")
			return
		}
		next(w, r)
	}
}

// SealUnlockReq is the JSON body for POST /v1/seal/unlock. The KEK is
// 32 bytes (AES-256-GCM key). Posted in the clear over loopback only.
type SealUnlockReq struct {
	KEK []byte `json:"kek"`
}

// SealUnlockResp confirms the unlock + reports which subsystem was
// affected. The "sync" field is true when a SyncClient is configured
// and now has a sealer installed.
type SealUnlockResp struct {
	Status string `json:"status"`
	Sync   bool   `json:"sync"`
}

func (d *Daemon) handleSealUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	defer r.Body.Close()
	var req SealUnlockReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorStatus(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	if len(req.KEK) != 32 {
		writeErrorStatus(w, http.StatusBadRequest, "kek must be 32 bytes")
		return
	}
	sealer, err := auth.NewAESGCMSealer(req.KEK)
	if err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, "build sealer: "+err.Error())
		return
	}
	if d.sync != nil {
		d.sync.SetSealer(sealer)
	}
	writeJSON(w, http.StatusOK, SealUnlockResp{Status: "unlocked", Sync: d.sync != nil})
}

// SealLockResp confirms the lock + reports whether sync was parked.
type SealLockResp struct {
	Status string `json:"status"`
	Sync   bool   `json:"sync"`
}

func (d *Daemon) handleSealLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if d.sync != nil {
		d.sync.ClearSealer()
	}
	writeJSON(w, http.StatusOK, SealLockResp{Status: "locked", Sync: d.sync != nil})
}

// SealStatusResp is what GET /v1/seal/status returns. Used by doctor +
// the login CLI to verify the unlock actually took.
type SealStatusResp struct {
	Sealed    bool `json:"sealed"`     // true iff a Sealer is installed
	SyncReady bool `json:"sync_ready"` // sync configured AND has a sealer
}

func (d *Daemon) handleSealStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErrorStatus(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sealed := false
	syncReady := false
	if d.sync != nil {
		sealed = d.sync.getSealer() != nil
		syncReady = sealed
	}
	writeJSON(w, http.StatusOK, SealStatusResp{Sealed: sealed, SyncReady: syncReady})
}
