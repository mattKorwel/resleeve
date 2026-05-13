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
	if err := fs.Parse(args); err != nil {
		return 2
	}

	d, err := agent.New(ctx, agent.Config{DSN: *dsn, Addr: *addr})
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
