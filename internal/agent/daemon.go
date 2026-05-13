package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

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
}

// Config holds daemon configuration.
type Config struct {
	DSN  string // sqlite DSN
	Addr string // listen address; ":0" for random port
}

// New opens the storage backend and prepares the daemon. Call Serve to
// bind the listener and begin handling requests.
func New(ctx context.Context, cfg Config) (*Daemon, error) {
	store, err := sqlite.Open(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	d := &Daemon{cfg: cfg, store: store}
	mux := http.NewServeMux()
	d.registerRoutes(mux)
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

	// Shutdown goroutine.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.server.Shutdown(shutCtx)
		_ = RemoveEndpoint(endpointPath)
		_ = d.store.Close()
	}()

	serveErr := d.server.Serve(ln)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}
