// Package agenttest provides test affordances for the agent daemon.
//
// It is the sibling testing subpackage for internal/agent — the
// standard Go pattern (cf. net/http/httptest, testing/fstest) for
// exposing testing helpers that need to import the package under test
// without leaking those helpers into the production package's
// compiled API. The agent package never imports agenttest; agenttest
// imports agent.
package agenttest

import (
	"context"
	"fmt"
	"net/http"

	"github.com/mattkorwel/resleeve/internal/agent"
)

// TestHandler returns an http.Handler wired to a brand-new sqlite store
// with all the daemon's routes registered, plus the bearer secret used
// to authorize requests. Intended for use with httptest.NewServer in
// downstream packages (e.g. internal/mcp) that need a real daemon to
// drive integration tests against without binding a TCP listener,
// writing endpoint files, or spawning reconcile goroutines.
//
// The returned cleanup function closes the underlying store.
func TestHandler(ctx context.Context, dsn, secret string) (http.Handler, func(), error) {
	if dsn == "" {
		dsn = "file::memory:?cache=shared&_pragma=foreign_keys=on"
	}
	if secret == "" {
		secret = "test-secret-for-mcp"
	}
	d, err := agent.New(ctx, agent.Config{DSN: dsn, Secret: secret})
	if err != nil {
		return nil, nil, fmt.Errorf("agent.New: %w", err)
	}
	cleanup := func() { _ = d.Close() }
	return d.Handler(), cleanup, nil
}
