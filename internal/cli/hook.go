package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/adapter/claude"
	"github.com/mattkorwel/resleeve/internal/adapter/registry"
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
	memoryOnly := fs.Bool("memory-only", false, "skip persisting events to the daemon; SessionStart still emits the additionalContext envelope so memory injection keeps working")
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
	if !*memoryOnly {
		_ = client.AppendEvents(ctx, sessionID, events)
	}

	// SessionStart side-channel: fetch the rolled-up memory for this
	// scope and emit the matcher-wrapped hookSpecificOutput envelope
	// Claude Code expects so the additionalContext gets injected into
	// the conversation. Silent no-op when no scope, no daemon-side
	// memory, or any error along the way (a hook failure must not
	// crash the harness).
	//
	// This runs in both modes — memory-only just skips the AppendEvents
	// above, but injection (the whole point of memory-only mode) stays.
	if events[0].Kind == event.KindSessionStart {
		injectContext(ctx, client, events[0].Slot.Scope)
	}
	return 0
}

// sessionStartHookOutput is the JSON envelope Claude Code expects from
// a SessionStart hook. Two layers:
//
//   - hookSpecificOutput.additionalContext is silently injected into
//     the system prompt (model sees it, user doesn't).
//   - systemMessage at the top level is a visible notice the user sees
//     in the CC UI when the hook fires — this is the breadcrumb that
//     tells the user "resleeve loaded N bytes of memory for scope X."
//
// The flat {"additionalContext": "..."} form (which a v1 dogfood
// iteration of this code emitted) is silently dropped by current CC
// versions — F13 fix introduced the wrapping; F14 added systemMessage
// after dogfood revealed users had no signal anything had loaded.
type sessionStartHookOutput struct {
	SystemMessage      string             `json:"systemMessage,omitempty"`
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
	// Also persist what we just injected, so users can `cat
	// ~/.resleeve/last-injected.md` to audit exactly what reached the
	// model. Best-effort — write errors don't suppress the hook.
	writeLastInjected(scope, body)
	out := sessionStartHookOutput{
		SystemMessage: fmt.Sprintf("resleeve: loaded scope %q (%d bytes)", scope, len(body)),
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

// writeLastInjected persists the most recent SessionStart injection to
// ~/.resleeve/last-injected.md. Lets users audit exactly what reached
// the model post-hoc (`cat ~/.resleeve/last-injected.md`). Best-effort:
// errors are swallowed since this is a debug breadcrumb, not a contract.
func writeLastInjected(scope, body string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".resleeve")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, "last-injected.md")
	header := fmt.Sprintf("<!-- scope: %s\n     injected at: %s -->\n\n", scope, time.Now().UTC().Format(time.RFC3339))
	_ = os.WriteFile(path, []byte(header+body), 0o600)
}

// pickAdapter returns an adapter.Adapter for the given name, resolved
// through internal/adapter/registry (adapters self-register from their
// init(); see internal/cli/adapters.go). An empty name defaults to
// claude, preserving the hook flag's historical default.
func pickAdapter(name string) (adapter.Adapter, error) {
	if name == "" {
		name = claude.Name
	}
	a, ok := registry.New(name)
	if !ok {
		return nil, &unknownAdapterError{name: name}
	}
	return a, nil
}

type unknownAdapterError struct{ name string }

func (e *unknownAdapterError) Error() string {
	return "unknown adapter: " + e.name
}
