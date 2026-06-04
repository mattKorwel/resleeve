package cli

import (
	"context"
	"fmt"
	"os"
)

// runPair dispatches the `pair invite` and `pair accept` subcommands.
// Full implementation lands in commit 3 of punch-list #3.
func runPair(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: resleeve pair <invite|accept> [args]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "invite":
		return runPairInvite(ctx, rest)
	case "accept":
		return runPairAccept(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "resleeve pair: unknown subcommand %q\n", sub)
		return 2
	}
}

// runPairInvite is wired in commit 3 of #3.
func runPairInvite(ctx context.Context, args []string) int {
	fmt.Fprintln(os.Stderr, "resleeve pair invite: not yet implemented in this commit (lands in #3 commit 3)")
	return 1
}

// runPairAccept is wired in commit 3 of #3.
func runPairAccept(ctx context.Context, args []string) int {
	fmt.Fprintln(os.Stderr, "resleeve pair accept: not yet implemented in this commit (lands in #3 commit 3)")
	return 1
}
