package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
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
	var masterKeyFile string
	var maxPushBytes int64
	var maxSSEPerUser int
	var maxBrainsPerUser int
	fs.StringVar(&addr, "addr", "127.0.0.1:7860", "listen address")
	fs.StringVar(&root, "root", "", "blob storage root (default: ~/.local/share/resleeve/serve)")
	fs.StringVar(&token, "auth-token", "", "legacy single bearer token (default: $RESLEEVE_SERVE_TOKEN; empty disables legacy auth — per-device only)")
	fs.StringVar(&dsn, "dsn", "", "sqlite DSN for the identity database (default: ~/.local/share/resleeve/serve/identity.db)")
	fs.BoolVar(&singleTenant, "single-tenant", false, "solo self-host mode: no brain partitioning, legacy bearer accepted on /v2/sync/* (tiers 1–2)")
	fs.StringVar(&masterKeyFile, "master-key", "", "file holding the 32-byte server-at-rest master key (hex or base64; default: $RESLEEVE_SERVER_MASTER_KEY; absent = at-rest encryption disabled)")
	// round-15 multi-tenant DoS caps. 0 = server default (see serve.Config).
	fs.Int64Var(&maxPushBytes, "max-push-bytes", 0, "max decoded POST /v2/sync/push body in bytes (0 = default 32 MiB); over the cap returns 413")
	fs.IntVar(&maxSSEPerUser, "max-sse-per-user", 0, "max concurrent SSE connections per user (0 = default 16); over the cap returns 429")
	fs.IntVar(&maxBrainsPerUser, "max-brains-per-user", 0, "max brains a single user may own (0 = default 100); over the cap returns 429")

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

	// Server-at-rest (round-12 Part A): load the optional operator master
	// key from --master-key <file> or $RESLEEVE_SERVER_MASTER_KEY (hex or
	// base64, 32 bytes). When absent we skip at-rest encryption entirely.
	masterKey, err := loadMasterKey(masterKeyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve: master key:", err)
		return 1
	}
	// Round-12 Part A slice 2 (default-flip): a multi-tenant server has its
	// clients send PLAINTEXT under the default server-side policy, so the
	// server's at-rest DEK is the SOLE encryption layer — refuse to boot
	// without a master key rather than persisting plaintext. Single-tenant
	// solo self-hosters keep zero-knowledge client sealing, so the key
	// stays optional there. (serve.New enforces the same invariant; this
	// pre-check surfaces the actionable flag/env hint.)
	if !singleTenant && len(masterKey) == 0 {
		fmt.Fprintln(os.Stderr, "serve: multi-tenant serve requires --master-key / $RESLEEVE_SERVER_MASTER_KEY")
		fmt.Fprintln(os.Stderr, "       (clients send plaintext under the default server-side policy; the server's at-rest key is the only encryption layer)")
		fmt.Fprintln(os.Stderr, "       pass --single-tenant for a solo self-host (zero-knowledge client sealing, master key optional)")
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
		BrainKeys:    store.BrainKeys(),
		Memberships:  store.Memberships(),
		SingleTenant: singleTenant,
		MasterKey:    masterKey,
		// round-15 DoS caps (0 = serve.Config default).
		MaxPushBytes:     maxPushBytes,
		MaxSSEPerUser:    maxSSEPerUser,
		MaxBrainsPerUser: maxBrainsPerUser,
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

	// Bind explicitly so we can report the resolved address — when --addr
	// uses :0 the OS assigns the port, and logging the requested addr would
	// print ":0" instead of the port the operator needs to connect to.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	}
	addr = ln.Addr().String()

	fmt.Fprintf(os.Stderr, "resleeve serve listening on http://%s\n", addr)
	if !addrIsLoopback(addr) {
		fmt.Fprintf(os.Stderr, "  WARNING  --addr %s is not loopback — operator: confirm you've fronted this with TLS + auth\n", addr)
	}
	fmt.Fprintf(os.Stderr, "  backend  local-disk at %s\n", backend.Root())
	fmt.Fprintln(os.Stderr, "  auth     bearer (token gated)")
	fmt.Fprintln(os.Stderr, "  routes   POST /v2/sync/push  GET /v2/sync/pull  GET /v2/sync/sse  GET /v2/sync/health")
	fmt.Fprintln(os.Stderr, "           POST /v2/auth/register  /v2/auth/login-challenge  /v2/auth/login  /v2/auth/logout")
	fmt.Fprintln(os.Stderr, "           POST /v2/auth/pair/publish  /v2/auth/pair/claim")
	fmt.Fprintln(os.Stderr, "           POST/GET /v1/brains  GET/POST/DELETE /v1/brains/{id}/members")
	if singleTenant {
		fmt.Fprintln(os.Stderr, "  tenancy  single-tenant (no brain partitioning; legacy bearer accepted)")
	} else {
		fmt.Fprintln(os.Stderr, "  tenancy  multi-tenant (brain-scoped; per-device bearer required on /v2/sync/*)")
	}
	if len(masterKey) == 32 {
		fmt.Fprintln(os.Stderr, "  at-rest  server-side envelope encryption ON (per-brain DEKs wrapped under master key)")
	} else {
		fmt.Fprintln(os.Stderr, "  at-rest  DISABLED (no --master-key / $RESLEEVE_SERVER_MASTER_KEY); blobs stored as received")
	}

	// Graceful shutdown on context cancel.
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
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

// loadMasterKey resolves the optional server-at-rest master key (round-12
// Part A). Source precedence: --master-key <file> if non-empty, else
// $RESLEEVE_SERVER_MASTER_KEY. The value (file contents or env var) is
// trimmed and decoded as hex first, then base64; the result must be
// exactly 32 bytes (AES-256). Returns (nil, nil) when neither source is
// set — at-rest encryption is then disabled. Never logs the key bytes.
func loadMasterKey(file string) ([]byte, error) {
	var raw string
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read --master-key file: %w", err)
		}
		raw = string(b)
	default:
		raw = os.Getenv("RESLEEVE_SERVER_MASTER_KEY")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	key, err := decodeKeyMaterial(raw)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d (accepts 64 hex chars or base64)", len(key))
	}
	return key, nil
}

// decodeKeyMaterial decodes s as hex, falling back to base64 (standard
// then raw URL). Returns an error only when neither yields bytes.
func decodeKeyMaterial(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, errors.New("master key is neither valid hex nor base64")
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
