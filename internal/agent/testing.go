package agent

import (
	"context"
	"fmt"
	"net/http"

	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
)

// TestHandler returns an http.Handler wired to a brand-new sqlite store
// with all the daemon's routes registered, plus the bearer secret used
// to authorize requests. Intended for use with httptest.NewServer in
// downstream packages (e.g. internal/mcp) that need a real daemon to
// drive integration tests against without binding a TCP listener,
// writing endpoint files, or spawning reconcile goroutines.
//
// The returned cleanup function closes the underlying store.
//
// This is a testing affordance, not a runtime API. It lives in a
// non-`_test.go` file because cross-package tests can't import
// internal symbols defined only in test files.
func TestHandler(ctx context.Context, dsn, secret string) (http.Handler, func(), error) {
	if dsn == "" {
		dsn = "file::memory:?cache=shared&_pragma=foreign_keys=on"
	}
	if secret == "" {
		secret = "test-secret-for-mcp"
	}
	store, err := sqlite.Open(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	d := &Daemon{cfg: Config{DSN: dsn}, store: store, secret: secret}
	mux := http.NewServeMux()
	d.registerRoutes(mux)
	d.registerMemoryRoutes(mux)
	cleanup := func() { _ = store.Close() }
	return mux, cleanup, nil
}
