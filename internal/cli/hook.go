package cli

import (
	"context"
	"flag"
	"io"
	"os"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/claude"
	"github.com/mattkorwel/resleeve/internal/agent"
)

// runHook is the "resleeve hook" subcommand. Claude Code's hook system
// invokes it with the hook event JSON on stdin. It must always exit 0
// (per docs/design/round-2/02-journey-01-decisions.md Q10) — a hook
// failure must never crash the harness.
func runHook(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	adapterName := fs.String("adapter", "claude", "adapter name")
	if err := fs.Parse(args); err != nil {
		return 0
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 0
	}

	endpoint, secret, err := agent.LoadEndpoint()
	if err != nil {
		// Silent no-op when no daemon resolves — don't crash Claude Code.
		return 0
	}

	a, err := pickAdapter(*adapterName)
	if err != nil {
		return 0
	}

	events, err := a.FromNative(ctx, raw, adapter.Source{Kind: adapter.SourceHook})
	if err != nil || len(events) == 0 {
		return 0
	}

	sessionID := events[0].SessionID
	if sessionID == "" {
		return 0
	}

	client := agent.NewClient(endpoint, secret)
	_ = client.AppendEvents(ctx, sessionID, events)
	return 0
}

// pickAdapter returns an adapter.Adapter for the given name. v1 only
// supports claude; v3 will register opencode / codex / gemini.
func pickAdapter(name string) (adapter.Adapter, error) {
	switch name {
	case claude.Name, "":
		return claude.New(), nil
	default:
		return nil, &unknownAdapterError{name: name}
	}
}

type unknownAdapterError struct{ name string }

func (e *unknownAdapterError) Error() string {
	return "unknown adapter: " + e.name
}
