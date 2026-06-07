package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/serve"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

// runServe implements `resleeve serve [--addr] [--root] [--auth-token]`.
// Boots the v2 sync HTTP server with a local-disk backend. v2 slice 1
// stub: single-token auth, no identity, no SSE. See
// docs/design/round-4/02-cross-machine-sync.md.
func runServe(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		addr  string
		root  string
		token string
	)
	var dsn string
	var singleTenant bool
	fs.StringVar(&addr, "addr", "127.0.0.1:7860", "listen address")
	fs.StringVar(&root, "root", "", "blob storage root (default: ~/.local/share/resleeve/serve)")
	fs.StringVar(&token, "auth-token", "", "legacy single bearer token (default: $RESLEEVE_SERVE_TOKEN; empty disables legacy auth — per-device only)")
	fs.StringVar(&dsn, "dsn", "", "sqlite DSN for the identity database (default: ~/.local/share/resleeve/serve/identity.db)")
	fs.BoolVar(&singleTenant, "single-tenant", false, "solo self-hoster mode: disable brain keyspace partitioning and keep the legacy no-user bearer valid on /v2/sync/* (default: multi-tenant — sync is brain-scoped and the legacy bearer is rejected)")

	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "serve: resolve homedir:", err)
			return 1
		}
		root = filepath.Join(home, ".local", "share", "resleeve", "serve")
	}
	if dsn == "" {
		if err := os.MkdirAll(root, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "serve: mkdir root:", err)
			return 1
		}
		dsn = "file:" + filepath.Join(root, "identity.db") + "?_pragma=journal_mode=WAL&_pragma=foreign_keys=on"
	}

	if token == "" {
		token = os.Getenv("RESLEEVE_SERVE_TOKEN")
	}
	// New behavior: identity (per-device tokens) is always wired. If
	// no legacy bearer is set, register/login mint the only tokens
	// that work; new deployments don't need RESLEEVE_SERVE_TOKEN.
	// We still print a generated legacy bearer when no identity store
	// has any devices yet, so first-run smoke (`resleeve up --upstream`)
	// keeps working without a register step.
	store, err := sqlite.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve: open identity db:", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	if token == "" {
		gen, err := generateToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "serve: generate token:", err)
			return 1
		}
		token = gen
		fmt.Fprintf(os.Stderr, "serve: generated bearer token (one-time): %s\n", token)
		fmt.Fprintln(os.Stderr, "       set RESLEEVE_SERVE_TOKEN or --auth-token to use a stable value")
		fmt.Fprintln(os.Stderr, "       (or use `resleeve register` + per-device tokens)")
	}

	backend, err := local.New(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve: backend:", err)
		return 1
	}

	srv, err := serve.New(serve.Config{
		Backend:      backend,
		AuthToken:    token,
		ServerUsers:  store.ServerUsers(),
		Devices:      store.Devices(),
		Pairings:     store.Pairings(),
		ServeMeta:    store.ServeMeta(),
		Brains:       store.Brains(),
		Memberships:  store.Memberships(),
		SingleTenant: singleTenant,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "resleeve serve listening on http://%s\n", addr)
	if !addrIsLoopback(addr) {
		fmt.Fprintf(os.Stderr, "  WARNING  --addr %s is not loopback — operator: confirm you've fronted this with TLS + auth\n", addr)
	}
	fmt.Fprintf(os.Stderr, "  backend  local-disk at %s\n", backend.Root())
	fmt.Fprintln(os.Stderr, "  auth     bearer (token gated)")
	if singleTenant {
		fmt.Fprintln(os.Stderr, "  tenancy  single-tenant (no brain partitioning; legacy bearer valid on /v2/sync/*)")
	} else {
		fmt.Fprintln(os.Stderr, "  tenancy  multi-tenant (brain-scoped keyspace; per-device token required on /v2/sync/*)")
	}
	fmt.Fprintln(os.Stderr, "  routes   POST /v2/sync/push  GET /v2/sync/pull  GET /v2/sync/sse  GET /v2/sync/health")
	fmt.Fprintln(os.Stderr, "           POST /v2/auth/register  /v2/auth/login-challenge  /v2/auth/login  /v2/auth/logout")
	fmt.Fprintln(os.Stderr, "           POST /v2/auth/pair/publish  /v2/auth/pair/claim")

	// Graceful shutdown on context cancel.
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintln(os.Stderr, "serve: shutdown:", err)
		}
		return 0
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, "serve: listen:", err)
		return 1
	}
}

// generateToken produces a 32-byte random hex token for bearer auth
// when the operator didn't supply one.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// addrIsLoopback reports whether a listen address binds the loopback
// interface only. Anything else gets a startup-warning so the operator
// confirms they've fronted resleeve serve with TLS + auth. Unparseable
// addresses (or bare port forms like ":7860") are treated as NON-loopback
// — the safe default for "I'm not sure" is "warn the operator".
func addrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
