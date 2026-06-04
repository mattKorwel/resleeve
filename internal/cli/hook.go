package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/claude"
	"github.com/mattkorwel/resleeve/internal/agent"
	"github.com/mattkorwel/resleeve/internal/event"
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

	// SessionStart side-channel: fetch the rolled-up memory for this
	// scope and emit the matcher-wrapped hookSpecificOutput envelope
	// Claude Code expects so the additionalContext gets injected into
	// the conversation. Silent no-op when no scope, no daemon-side
	// memory, or any error along the way (a hook failure must not
	// crash the harness).
	if events[0].Kind == event.KindSessionStart {
		injectContext(ctx, client, events[0].Slot.Scope)
	}
	return 0
}

// sessionStartHookOutput is the JSON envelope Claude Code expects from
// a SessionStart hook to inject additionalContext into the conversation.
// Shape comes from Claude Code's hook contract: the flat
// {"additionalContext": "..."} form (which a v1 dogfood iteration of
// this code emitted) is silently dropped by current CC versions.
type sessionStartHookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

func injectContext(ctx context.Context, c *agent.Client, scope string) {
	if scope == "" || scope == "unknown" {
		return
	}
	body, err := c.GetContext(ctx, scope)
	if err != nil || body == "" {
		return
	}
	out := sessionStartHookOutput{
		HookSpecificOutput: hookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: body,
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	fmt.Println(string(b))
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
