package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattkorwel/resleeve/internal/agent"
	"github.com/mattkorwel/resleeve/internal/auth"
)

func runAgent(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	dsn := fs.String("dsn", defaultDSN(), "sqlite DSN")
	addr := fs.String("addr", "127.0.0.1:0", "listen address; :0 picks a random port")
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	upstreamToken := fs.String("upstream-token", "", "v2 sync bearer token (default: $RESLEEVE_UPSTREAM_TOKEN)")
	// Punch-list #5 retires the placeholder seal.key model: the daemon
	// no longer auto-generates ~/.resleeve/seal.key. New installs unlock
	// the sealer at runtime via `resleeve login`. Legacy users with a
	// pre-existing seal.key can opt in to it for back-compat by passing
	// --seal-key=<path>; this is a transition shim and `resleeve doctor`
	// will nudge toward migration. See `resleeve migrate-key`.
	sealKey := fs.String("seal-key", "", "legacy: load placeholder AES key from this path (transition shim; prefer `resleeve login`)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *upstream == "" {
		*upstream = os.Getenv("RESLEEVE_UPSTREAM")
	}
	if *upstreamToken == "" {
		*upstreamToken = os.Getenv("RESLEEVE_UPSTREAM_TOKEN")
	}

	// Default startup: no Sealer. The sealer stays nil until
	// `resleeve login` POSTs a derived KEK to /v1/seal/unlock. Sync loops
	// park in the meantime. Capture continues locally (Enqueue still
	// writes to outbox; the drain side is what parks).
	//
	// --seal-key=PATH is the back-compat opt-in for users on the
	// pre-round-5 placeholder model. We only READ — never auto-generate.
	// A missing file is an explicit error so an operator notices the
	// drift rather than silently flipping into login-only mode.
	var sealer auth.Sealer
	if *sealKey != "" {
		key, err := os.ReadFile(*sealKey)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resleeve agent: --seal-key:", err)
			return 1
		}
		if len(key) != 32 {
			fmt.Fprintf(os.Stderr, "resleeve agent: --seal-key %s: length %d, want 32\n", *sealKey, len(key))
			return 1
		}
		s, err := auth.NewAESGCMSealer(key)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resleeve agent: sealer:", err)
			return 1
		}
		sealer = s
		fmt.Fprintln(os.Stderr, "resleeve agent: legacy --seal-key loaded; consider `resleeve migrate-key` to switch to the master-password KEK")
	}

	d, err := agent.New(ctx, agent.Config{
		DSN:           *dsn,
		Addr:          *addr,
		Upstream:      *upstream,
		UpstreamToken: *upstreamToken,
		Sealer:        sealer,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "resleeve agent:", err)
		return 1
	}
	if err := d.Serve(ctx); err != nil && err != context.Canceled {
		fmt.Fprintln(os.Stderr, "resleeve agent:", err)
		return 1
	}
	return 0
}

func defaultDSN() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	dbPath := filepath.Join(home, ".resleeve", "data.db")
	return "file:" + dbPath + "?_pragma=journal_mode=WAL&_pragma=foreign_keys=on"
}

// defaultSealKeyPath returns the well-known path of the (now-legacy)
// placeholder seal key. The agent no longer auto-loads from here, but
// `resleeve doctor` and `resleeve migrate-key` check this path to detect
// users still on the pre-round-5 model.
func defaultSealKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".resleeve", "seal.key")
}
