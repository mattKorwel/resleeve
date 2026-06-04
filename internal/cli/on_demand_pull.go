package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/agent"
)

// defaultOnDemandPullTimeout caps how long we'll block the user's verb
// (resume / session show / session list) waiting on an upstream pull
// before falling back to the local view. Kept tight on purpose: a slow
// upstream must NOT make every command feel sluggish (punch-list #2
// gotcha). 2s is the value called out in the design brief.
const defaultOnDemandPullTimeout = 2 * time.Second

// syncPuller is the subset of *agent.Client that tryOnDemandPull needs.
// Extracted so the helper can be exercised by table-driven tests without
// standing up a live daemon.
type syncPuller interface {
	SyncPullNow(ctx context.Context) (*agent.SyncPullResp, error)
}

// tryOnDemandPull runs one best-effort pull from upstream before a
// read-heavy verb (resume / session show / session list) resolves
// state. Behaviour:
//
//   - Success → caller proceeds against fresh local state.
//   - Timeout / network / daemon error → warn on stderr ("upstream
//     unreachable; showing local state") and return; caller proceeds
//     against the existing local view (no abort).
//   - Daemon returns 409 "no upstream configured" → the user is running
//     standalone, this is NOT an error; silently skip.
//
// The timeout is applied via context.WithTimeout, derived from the
// caller's ctx so user-level cancellation (Ctrl-C) still propagates.
func tryOnDemandPull(ctx context.Context, c syncPuller, timeout time.Duration, warnTo io.Writer) {
	if c == nil {
		return
	}
	pullCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	_, err := c.SyncPullNow(pullCtx)
	if err == nil {
		return
	}
	if isNoUpstreamConfigured(err) {
		// Standalone deployment — no upstream to pull from. Not an
		// error condition for the user; stay silent.
		return
	}
	// Everything else (timeout, network, partial failure, auth) is
	// non-fatal: log a one-liner and continue with local state.
	fmt.Fprintln(warnTo, "(upstream unreachable; showing local state)")
}

// isNoUpstreamConfigured returns true if err is the daemon's 409
// response for "no upstream configured". The agent client surfaces the
// HTTP status + body as a formatted string ("daemon 409 Conflict: ...");
// we match on the body fragment because it's stable across handlers
// (see internal/agent/sync_handlers.go).
func isNoUpstreamConfigured(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no upstream configured")
}
