package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattkorwel/resleeve/internal/agent"
)

func runAgent(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	dsn := fs.String("dsn", defaultDSN(), "sqlite DSN")
	addr := fs.String("addr", "127.0.0.1:0", "listen address; :0 picks a random port")
	upstream := fs.String("upstream", "", "v2 sync upstream URL (default: $RESLEEVE_UPSTREAM)")
	upstreamToken := fs.String("upstream-token", "", "v2 sync bearer token (default: $RESLEEVE_UPSTREAM_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *upstream == "" {
		*upstream = os.Getenv("RESLEEVE_UPSTREAM")
	}
	if *upstreamToken == "" {
		*upstreamToken = os.Getenv("RESLEEVE_UPSTREAM_TOKEN")
	}

	d, err := agent.New(ctx, agent.Config{
		DSN:           *dsn,
		Addr:          *addr,
		Upstream:      *upstream,
		UpstreamToken: *upstreamToken,
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
