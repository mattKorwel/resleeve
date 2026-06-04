package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter/claude"
	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
)

// Daemon is the local resleeve agent process. It owns the SQLite
// store, an HTTP listener on loopback, and the endpoint file used by
// bridge plugins to discover it.
type Daemon struct {
	cfg      Config
	store    *sqlite.Store
	server   *http.Server
	listener net.Listener
	endpoint string
	secret   string
	sync     *SyncClient // nil when no --upstream is configured
}

// Config holds daemon configuration.
type Config struct {
	DSN           string // sqlite DSN
	Addr          string // listen address; ":0" for random port
	Upstream      string // v2 sync: base URL of resleeve serve (empty = standalone, no sync)
	UpstreamToken string // bearer token presented to upstream (empty allowed if Upstream is empty)

	// Sealer, when non-nil and Upstream is set, encrypts outbox blobs
	// before push and decrypts pulled blobs before ingest. Slice 2.5
	// uses a daemon-local random key; round 5+ swaps in a KEK derived
	// from the user's master password.
	Sealer auth.Sealer

	// SealKeyPath is where the CLI persists the placeholder seal key.
	// Informational only on Config — the CLI resolves it and builds
	// Sealer before calling New.
	SealKeyPath string
}

// New opens the storage backend and prepares the daemon. Call Serve to
// bind the listener and begin handling requests.
func New(ctx context.Context, cfg Config) (*Daemon, error) {
	store, err := sqlite.Open(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	d := &Daemon{cfg: cfg, store: store}
	if cfg.Upstream != "" {
		if cfg.Sealer != nil {
			d.sync = NewSyncClientWithSealer(store, cfg.Upstream, cfg.UpstreamToken, cfg.Sealer)
		} else {
			d.sync = NewSyncClient(store, cfg.Upstream, cfg.UpstreamToken)
		}
	}
	mux := http.NewServeMux()
	d.registerRoutes(mux)
	d.registerMemoryRoutes(mux)
	d.registerSyncRoutes(mux)
	d.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return d, nil
}

// Serve binds the listener, writes the endpoint file, and serves
// requests until ctx is canceled. On shutdown it removes the endpoint
// file and closes the store.
func (d *Daemon) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", d.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.cfg.Addr, err)
	}
	d.listener = ln

	secret, err := generateSecret()
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("generate secret: %w", err)
	}
	d.secret = secret

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	endpointPath, err := WriteEndpoint(url, secret)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("write endpoint: %w", err)
	}
	d.endpoint = endpointPath
	fmt.Printf("resleeve agent listening on %s\nendpoint: %s\n", url, endpointPath)
	if d.sync != nil {
		fmt.Printf("sync upstream: %s\n", d.cfg.Upstream)
		d.sync.Start(ctx)
	}

	// Write PID file so `resleeve down` / `doctor` can find this daemon.
	pidPath, _ := PIDPath()
	if pidPath != "" {
		_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	}

	// Shutdown goroutine.
	go func() {
		<-ctx.Done()
		if d.sync != nil {
			d.sync.Stop()
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.server.Shutdown(shutCtx)
		_ = RemoveEndpoint(endpointPath)
		if pidPath != "" {
			_ = os.Remove(pidPath)
		}
		_ = d.store.Close()
	}()

	// One-shot reconcile sweep over Claude Code's session JSONL files.
	// Backfills any events the live hook path missed. Deterministic
	// UUIDs + INSERT OR IGNORE handle dedup against already-captured rows.
	go func() {
		a := claude.New()
		if err := a.ReconcileOnce(ctx, d); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("reconcile: %v", err)
		}
	}()

	serveErr := d.server.Serve(ln)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}
