package cli

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/mattkorwel/resleeve/internal/agent"
	"github.com/mattkorwel/resleeve/internal/event"
)

// runHook is the "resleeve hook" subcommand. Reads a Claude Code hook
// JSON envelope on stdin, wraps it as a single KindSystem event for
// Stage 3a (real per-record events arrive in 3b), and POSTs to the
// daemon. Always exits 0 — a hook failure must NOT crash Claude Code.
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
		// Silent no-op when no daemon resolves (per 02-journey-01-decisions.md Q10).
		return 0
	}

	// Probe session metadata from the hook envelope.
	var probe struct {
		SessionID     string `json:"session_id"`
		HookEventName string `json:"hook_event_name"`
		Cwd           string `json:"cwd"`
	}
	_ = json.Unmarshal(raw, &probe)

	sessionID := probe.SessionID
	if sessionID == "" {
		sessionID = "unknown-" + uuid.NewString()
	}

	now := time.Now().UTC()
	contentBytes, _ := json.Marshal(map[string]any{
		"raw_hook":        json.RawMessage(raw),
		"hook_event_name": probe.HookEventName,
		"cwd":             probe.Cwd,
		"received_at":     now.Format(time.RFC3339Nano),
	})

	scope := "unknown"
	if probe.Cwd != "" {
		scope = filepath.Base(probe.Cwd)
	}

	e := event.Event{
		EventUUID:     uuid.NewString(),
		SessionID:     sessionID,
		Slot:          event.Slot{Scope: scope, AgentName: "default"},
		Seq:           now.UnixNano(),
		Timestamp:     now,
		Kind:          event.KindSystem,
		SchemaVersion: 1,
		Content:       contentBytes,
		Vendor:        event.Vendor{Name: *adapterName, Version: "unknown"},
	}

	client := agent.NewClient(endpoint, secret)
	_ = client.AppendEvents(ctx, sessionID, []event.Event{e})
	return 0
}
