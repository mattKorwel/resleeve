package agent

// Structured JSON error envelope shared by all `resleeve` daemon HTTP
// responses (Q2). Mirrors internal/serve/errors.go — keep the codes
// vocabulary in sync.
//
// Pre-Q2, daemon 4xx paths returned text/plain via http.Error and the
// client (Client.doJSON) had to substring-match the body to recognize
// ErrNoUpstream. Brittle: a wording tweak in the daemon silently broke
// the standalone-mode detection in `resleeve resume` (see the
// errors.go ErrNoUpstream learning at 2026-06-04).
//
// Post-Q2 both daemon and serve emit:
//
//	{"error": {"code": "<machine-readable>", "message": "<human>"}}
//
// and Client.doJSON parses the envelope to surface a typed error via
// the code field. Body-substring fallback is kept for back-compat in
// case the daemon's response is not JSON (genuinely unusual; mostly
// covers proxies + handler bugs).

import (
	"encoding/json"
	"net/http"
)

// errorEnvelope is the on-wire wrapper for 4xx/5xx responses from the
// local daemon. Unexported because callers should go through
// writeErrorEnvelope; the client side decodes into errorEnvelope
// directly in client.go.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// errorBody is the inner {code, message} object.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Stable codes emitted by daemon 4xx paths. Mirrors
// internal/serve/errors.go — see that file for the full vocabulary
// description; the daemon uses a subset (no pairing-specific codes).
const (
	// codeNoUpstream — daemon 409 when no SyncClient is wired. The
	// client wraps this into ErrNoUpstream so callers can branch via
	// errors.Is without string-matching the message body.
	codeNoUpstream       = "no_upstream"
	codeInvalidRequest   = "invalid_request"
	codeUnauthorized     = "unauthorized"
	codeForbidden        = "forbidden"
	codeNotFound         = "not_found"
	codeConflict         = "conflict"
	codeMethodNotAllowed = "method_not_allowed"
	codeInternal         = "internal"
)

// writeErrorEnvelope emits the structured envelope at the given HTTP
// status. Use this instead of http.Error for daemon 4xx/5xx responses
// so the client can parse the code without substring matching.
func writeErrorEnvelope(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: code, Message: msg}})
}

// defaultDaemonCodeForStatus picks a reasonable code for daemon call
// sites that don't (yet) specify one. Keeps the migration incremental:
// new code spells out the code explicitly; legacy http.Error sites can
// route through writeErrorStatus and get a sensible default.
func defaultDaemonCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return codeInvalidRequest
	case http.StatusUnauthorized:
		return codeUnauthorized
	case http.StatusForbidden:
		return codeForbidden
	case http.StatusNotFound:
		return codeNotFound
	case http.StatusMethodNotAllowed:
		return codeMethodNotAllowed
	case http.StatusConflict:
		return codeConflict
	default:
		if status >= 500 {
			return codeInternal
		}
		return codeInvalidRequest
	}
}

// writeErrorStatus is the drop-in replacement for http.Error that emits
// the structured envelope. The code is inferred from the HTTP status;
// call writeErrorEnvelope directly when you have a more specific code.
func writeErrorStatus(w http.ResponseWriter, status int, msg string) {
	writeErrorEnvelope(w, status, defaultDaemonCodeForStatus(status), msg)
}
