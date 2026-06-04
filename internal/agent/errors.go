package agent

import "errors"

// ErrNoUpstream is returned (wrapped) by Client methods when the local
// daemon responds with HTTP 409 + body "no upstream configured …" —
// i.e. the user is running standalone with no --upstream wired in.
//
// Callers should test for this with errors.Is so that string-format
// changes in the daemon's error body don't break the standalone-mode
// detection. The previous strings.Contains coupling in
// internal/cli/on_demand_pull.go was replaced for exactly that reason
// (round-5 follow-up F).
var ErrNoUpstream = errors.New("agent: no upstream configured")
