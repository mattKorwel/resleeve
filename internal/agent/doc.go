// Package agent is resleeve's local daemon — the loopback HTTP service
// that bridge plugins talk to. Random TCP port + XDG endpoint file for
// discovery (per docs/design/round-2/02-journey-01-decisions.md Q4).
// Buffers events; relays to upstream server when present.
// See docs/design/round-2/08-api-surface.md.
package agent
