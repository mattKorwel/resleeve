package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
)

// runSync dispatches `resleeve sync <push|pull>`. The verbs hit the
// daemon's manual-escape-hatch endpoints (POST /v1/sync/push-now,
// POST /v1/sync/pull-now) — useful when you don't want to wait for the
// background drain ticker (1s) or pull ticker (30s). See
// docs/design/round-4/03-rehydrate-ux.md §"New CLI verbs".
func runSync(ctx context.Context, args []string) int {
	if len(args) == 0 {
		printSyncUsage(os.Stderr)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "push":
		return runSyncPush(ctx, rest)
	case "pull":
		return runSyncPull(ctx, rest)
	case "help", "--help", "-h":
		printSyncUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "resleeve sync: unknown subcommand %q\n\n", sub)
		printSyncUsage(os.Stderr)
		return 2
	}
}

func printSyncUsage(w *os.File) {
	fmt.Fprintln(w, "usage: resleeve sync <push|pull>")
	fmt.Fprintln(w, "subcommands:")
	fmt.Fprintln(w, "  push   drain the local outbox to upstream immediately")
	fmt.Fprintln(w, "  pull   pull the latest rows from upstream immediately")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Both subcommands require the daemon to be running with --upstream")
	fmt.Fprintln(w, "configured (see `resleeve up --upstream URL --upstream-token TOK`).")
}

// runSyncPush hits POST /v1/sync/push-now. Accepts no flags today.
// FlagSet rejects unknown flags and unexpected positionals (caught
// after Parse) so typos like `resleeve sync push --force` fail early.
func runSyncPush(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("sync push", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: resleeve sync push")
		fmt.Fprintln(os.Stderr, "Drains the local outbox to upstream immediately.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "sync push: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync push:", err)
		return 1
	}
	resp, err := c.SyncPushNow(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync push:", err)
		return 1
	}
	if resp.Pushed == 0 {
		fmt.Println("nothing pending")
	} else {
		fmt.Printf("pushed %d %s\n", resp.Pushed, pluralize("row", resp.Pushed))
	}
	if resp.Error != "" {
		// Partial-failure path: drain reported an error after pushing
		// (some) rows. Print to stderr and exit 1 so scripts can tell.
		fmt.Fprintln(os.Stderr, "sync push: drain error:", resp.Error)
		return 1
	}
	return 0
}

// runSyncPull hits POST /v1/sync/pull-now.
func runSyncPull(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("sync pull", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: resleeve sync pull")
		fmt.Fprintln(os.Stderr, "Pulls the latest rows from upstream immediately.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "sync pull: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync pull:", err)
		return 1
	}
	resp, err := c.SyncPullNow(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync pull:", err)
		return 1
	}
	// Print kinds in stable order — map iteration is random.
	total := 0
	for _, kind := range []string{"sessions", "events", "memory"} {
		n := resp.Pulled[kind]
		total += n
	}
	if total == 0 {
		fmt.Println("nothing new upstream")
	} else {
		fmt.Printf("pulled %d sessions, %d events, %d memory\n",
			resp.Pulled["sessions"], resp.Pulled["events"], resp.Pulled["memory"])
	}
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, "sync pull: pull error:", resp.Error)
		return 1
	}
	return 0
}

func pluralize(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
