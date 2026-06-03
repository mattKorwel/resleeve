package cli

import (
	"context"
	"fmt"
	"os"
)

// runContext prints the rolled-up markdown context the bridge would
// inject into Claude Code via `additionalContext` on SessionStart.
func runContext(ctx context.Context, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: resleeve context <scope>")
		return 2
	}
	c, err := clientFromEndpoint()
	if err != nil {
		fmt.Fprintln(os.Stderr, "context:", err)
		return 1
	}
	out, err := c.GetContext(ctx, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "context:", err)
		return 1
	}
	if out == "" {
		fmt.Println("(no context — no plans or learnings on the inherited chain)")
		return 0
	}
	fmt.Print(out)
	return 0
}
