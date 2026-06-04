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
	noSealKey := fs.Bool("no-seal-key", false, "skip the placeholder ~/.resleeve/seal.key; require `resleeve login` to unlock the sealer at runtime")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *upstream == "" {
		*upstream = os.Getenv("RESLEEVE_UPSTREAM")
	}
	if *upstreamToken == "" {
		*upstreamToken = os.Getenv("RESLEEVE_UPSTREAM_TOKEN")
	}

	// When upstream is configured, the daemon needs a Sealer to encrypt
	// outbound + decrypt inbound sync blobs. Two ways to get one:
	//   - placeholder seal.key (default): daemon-local random AES key.
	//     Easy first-run; everything works without `resleeve login`.
	//   - --no-seal-key: skip the placeholder; the Sealer stays nil
	//     until `resleeve login` POSTs a derived KEK to /v1/seal/unlock.
	//     Sync loops park in the meantime.
	// Punch-list #3 introduces the --no-seal-key path; existing users
	// keep the placeholder until they opt in.
	var sealer auth.Sealer
	sealKeyPath := defaultSealKeyPath()
	if *upstream != "" && !*noSealKey {
		key, err := auth.LoadOrCreateSealKey(sealKeyPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resleeve agent: seal key:", err)
			return 1
		}
		s, err := auth.NewAESGCMSealer(key)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resleeve agent: sealer:", err)
			return 1
		}
		sealer = s
	}

	d, err := agent.New(ctx, agent.Config{
		DSN:           *dsn,
		Addr:          *addr,
		Upstream:      *upstream,
		UpstreamToken: *upstreamToken,
		Sealer:        sealer,
		SealKeyPath:   sealKeyPath,
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

func defaultSealKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".resleeve", "seal.key")
}
