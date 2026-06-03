package agent

import "time"

// timeNowNano is a thin wrapper that lets tests override "now" later.
// Inlined helper to avoid sprinkling time.Now().UnixNano() everywhere.
func timeNowNano() int64 { return time.Now().UTC().UnixNano() }
