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

	"github.com/mattkorwel/resleeve/internal/auth"
	"github.com/mattkorwel/resleeve/internal/storage/sql/sqlite"
)

// Daemon is the local resleeve agent process. It owns the SQLite
// store, an HTTP listener on loopback, and the endpoint file used by
// bridge plugins to discover it.
//
// The Sealer is intentionally runtime-mutable via SetSealer/ClearSealer
// (called by the /v1/seal/unlock and /v1/seal/lock handlers). This is
// what lets `resleeve login` install a freshly-derived KEK into a
// running daemon without a restart.
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
	// before push and decrypts pulled blobs before ingest. Round 5
	// retired the daemon-local seal.key placeholder; the sealer now
	// arrives at runtime via /v1/seal/unlock after `resleeve login`
	// derives the KEK from the master password. A non-nil Sealer here
	// is the legacy --seal-key=PATH back-compat path only.
	Sealer auth.Sealer

	// Reconcilers are fired once at startup (in their own goroutines)
	// after Serve binds the listener. Empty means "no startup sweep".
	// The CLI layer is the canonical place to register adapter-specific
	// reconcilers — daemon stays neutral. See Q12 / reconciler.go.
	Reconcilers []Reconciler

	// Secret pins the bearer token used to authorize requests. Empty
	// means Serve will generate a random token at startup (the
	// production path). Test code that fronts the daemon via Handler()
	// without calling Serve uses this to install a known token — see
	// internal/agent/agenttest. Q15 promoted this from a post-hoc
	// SetSecret seam to a constructor option.
	Secret string
}

// New opens the storage backend and prepares the daemon. Call Serve to
// bind the listener and begin handling requests.
func New(ctx context.Context, cfg Config) (*Daemon, error) {
	store, err := sqlite.Open(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	d := &Daemon{cfg: cfg, store: store, secret: cfg.Secret}
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
	d.registerDoctorRoutes(mux)
	d.registerSealRoutes(mux)
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

	// d.secret was seeded by New from Config.Secret. Generate a fresh
	// random one only if the caller didn't pin one (the production
	// path; cfg.Secret is set today only by agenttest, which never
	// reaches Serve).
	if d.secret == "" {
		secret, err := generateSecret()
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("generate secret: %w", err)
		}
		d.secret = secret
	}

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	endpointPath, err := WriteEndpoint(url, d.secret)
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

	// One-shot reconcile sweep per registered adapter. Backfills any
	// events the live hook path missed. Deterministic UUIDs +
	// INSERT OR IGNORE on (session_id, event_uuid) make this safe
	// against rows the live hook already captured. The daemon is
	// adapter-neutral here: the CLI layer registers concrete
	// reconcilers (e.g. claude) via Config.Reconcilers — see Q12.
	for i, rec := range d.cfg.Reconcilers {
		i, rec := i, rec
		go func() {
			if err := rec(ctx, d); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("reconcile[%d]: %v", i, err)
			}
		}()
	}

	serveErr := d.server.Serve(ln)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

// Handler returns the daemon's HTTP handler (the mux with all routes
// registered). Useful for fronting the daemon with httptest.Server in
// downstream tests without binding a TCP listener. Note: the handler
// is constructed by New, so this is non-nil after New returns.
func (d *Daemon) Handler() http.Handler {
	return d.server.Handler
}

// Close releases the daemon's storage handle. Normally Serve handles
// cleanup via its shutdown goroutine; callers that use Handler()
// without Serve must call Close themselves.
func (d *Daemon) Close() error {
	return d.store.Close()
}
