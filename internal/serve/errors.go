package serve

// Structured JSON error envelope shared by all `resleeve serve` 4xx/5xx
// responses (Q2). Pre-Q2 the server returned a flat
// `{"error": "freeform text"}` and `resleeve` daemons returned plain
// text/plain via http.Error — clients had to string-match the body to
// detect specific failure modes (notably ErrNoUpstream from the daemon's
// 409). The envelope below decouples the wire from the prose so callers
// can branch on `code` instead.
//
// Wire shape:
//
//	{"error": {"code": "<machine-readable>", "message": "<human>"}}
//
// Both serve and the daemon emit the same shape — see also
// internal/agent/errors_envelope.go. Codes are deliberately stable
// strings (not enums); the client matches with errors.Is via the typed
// sentinels in internal/agent/errors.go.

import "net/http"

// Error is the inner object inside the {error: ...} envelope. Exported
// so test code in the same module can construct expected values.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorEnvelope is the top-level wire wrapper. Kept unexported because
// callers should go through writeErrorCoded / writeError, not build the
// envelope directly.
type errorEnvelope struct {
	Error Error `json:"error"`
}

// Stable error codes for the 4xx (+ 503 used by auth_handlers) paths
// emitted by serve and the daemon. Keep these in sync with
// internal/agent/errors_envelope.go — they're the cross-process
// vocabulary clients branch on.
const (
	// CodeNoUpstream — daemon 409, no --upstream configured. The client
	// wraps this into agent.ErrNoUpstream so `errors.Is` works.
	CodeNoUpstream = "no_upstream"
	// CodeInvalidRequest — 400. Decode failure, missing required query
	// param, malformed body, validation failure. Catch-all for "the
	// caller's request was structurally wrong".
	CodeInvalidRequest = "invalid_request"
	// CodeUnauthorized — 401. Missing/bad bearer or, for /v2/auth/login,
	// failed verifier compare.
	CodeUnauthorized = "unauthorized"
	// CodeForbidden — 403. Loopback-only routes hit from off-loopback.
	CodeForbidden = "forbidden"
	// CodeNotFound — 404. Resource lookup missed.
	CodeNotFound = "not_found"
	// CodeConflict — 409 catchall (email already registered, pair-code
	// already used, scope-has-children). Pair with the more specific
	// CodeNoUpstream when it's the upstream-missing path.
	CodeConflict = "conflict"
	// CodeGone — 410. Used by pair/claim for codes that were already
	// claimed or hit the TTL/lockout sweep.
	CodeGone = "gone"
	// CodeMethodNotAllowed — 405. Catch-all for the daemon's many
	// "method not allowed" branches.
	CodeMethodNotAllowed = "method_not_allowed"
	// CodeServiceUnavailable — 503. Used by /v2/auth/* when the identity
	// store isn't configured on the server.
	CodeServiceUnavailable = "service_unavailable"
	// CodePayloadTooLarge — 413. round-15 (M-B): the push body exceeded the
	// MaxPushBytes cap.
	CodePayloadTooLarge = "payload_too_large"
	// CodeTooManyRequests — 429. round-15 (M-C): a per-user DoS cap was hit
	// (concurrent SSE connections, owned brains).
	CodeTooManyRequests = "too_many_requests"
	// CodeInternal — 500 catchall.
	CodeInternal = "internal"
)

// defaultCodeForStatus picks a reasonable code for callers (the legacy
// writeError signature) that didn't supply one. Lets the rest of the
// codebase migrate incrementally without forcing every callsite to spell
// out the code at the same time as the envelope rollout.
func defaultCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return CodeInvalidRequest
	case http.StatusUnauthorized:
		return CodeUnauthorized
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusMethodNotAllowed:
		return CodeMethodNotAllowed
	case http.StatusConflict:
		return CodeConflict
	case http.StatusGone:
		return CodeGone
	case http.StatusServiceUnavailable:
		return CodeServiceUnavailable
	case http.StatusRequestEntityTooLarge:
		return CodePayloadTooLarge
	case http.StatusTooManyRequests:
		return CodeTooManyRequests
	default:
		if status >= 500 {
			return CodeInternal
		}
		return CodeInvalidRequest
	}
}

// writeErrorCoded emits the envelope with an explicit code. Use this
// when the call site knows the machine-readable failure mode (e.g. the
// daemon's no-upstream 409).
func writeErrorCoded(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorEnvelope{Error: Error{Code: code, Message: msg}})
}
