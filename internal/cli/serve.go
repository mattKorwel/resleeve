package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mattkorwel/resleeve/internal/serve"
	"github.com/mattkorwel/resleeve/internal/sync/local"
)

// runServe implements `resleeve serve [--addr] [--root] [--auth-token]`.
// Boots the v2 sync HTTP server with a local-disk backend. v2 slice 1
// stub: single-token auth, no identity, no SSE. See
// docs/design/round-4/02-cross-machine-sync.md.
func runServe(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		addr    string
		root    string
		token   string
	)
	fs.StringVar(&addr, "addr", "127.0.0.1:7860", "listen address")
	fs.StringVar(&root, "root", "", "blob storage root (default: ~/.local/share/resleeve/serve)")
	fs.StringVar(&token, "auth-token", "", "bearer token clients must present (default: $RESLEEVE_SERVE_TOKEN; if empty, a fresh one is generated and printed)")

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

	if token == "" {
		token = os.Getenv("RESLEEVE_SERVE_TOKEN")
	}
	if token == "" {
		gen, err := generateToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "serve: generate token:", err)
			return 1
		}
		token = gen
		fmt.Fprintf(os.Stderr, "serve: generated bearer token (one-time): %s\n", token)
		fmt.Fprintln(os.Stderr, "       set RESLEEVE_SERVE_TOKEN or --auth-token to use a stable value")
	}

	backend, err := local.New(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve: backend:", err)
		return 1
	}

	srv, err := serve.New(serve.Config{Backend: backend, AuthToken: token})
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
	fmt.Fprintf(os.Stderr, "  backend  local-disk at %s\n", backend.Root())
	fmt.Fprintln(os.Stderr, "  auth     bearer (token gated)")
	fmt.Fprintln(os.Stderr, "  routes   POST /v2/sync/push  GET /v2/sync/pull  GET /v2/sync/health")

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
