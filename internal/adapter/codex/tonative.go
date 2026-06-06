package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// ToNative renders a captured event stream back into Codex's native
// on-disk format.
//
// Replay mode: emit one rollout JSONL line per event, preferring
// event.Vendor.NativePayload (verbatim rollout-line bytes) when present.
// Events without a native payload and no synthesizable native form are
// dropped — `codex resume` reconstructs session-start state from the
// filename + first session_meta line. Events are emitted sorted by
// (seq, event_uuid).
//
// Prime mode: synthesize a markdown opening prompt summarizing the
// captured activity (delegated to common.SynthesizePrime). Lossy by
// design — the cross-CLI path.
func (a *Adapter) ToNative(ctx context.Context, events []event.Event, mode adapter.RenderMode) ([]byte, error) {
	if mode == adapter.RenderModeAuto {
		mode = adapter.RenderModeReplay
	}
	switch mode {
	case adapter.RenderModeReplay:
		return a.toNativeReplay(events)
	case adapter.RenderModePrime:
		return a.toNativePrime(sessionViewFromEvents(events), events)
	default:
		return nil, fmt.Errorf("codex.ToNative: unknown mode %q", mode)
	}
}

// sessionViewFromEvents constructs a minimal SessionView from the events
// themselves, for ToNative-direct callers that don't have one.
func sessionViewFromEvents(events []event.Event) adapter.SessionView {
	var view adapter.SessionView
	for _, e := range events {
		if e.SessionID != "" {
			view.SessionID = e.SessionID
			break
		}
	}
	view.CLI = Name
	return view
}

func (a *Adapter) toNativeReplay(events []event.Event) ([]byte, error) {
	sorted := make([]event.Event, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Seq != sorted[j].Seq {
			return sorted[i].Seq < sorted[j].Seq
		}
		return sorted[i].EventUUID < sorted[j].EventUUID
	})

	var buf bytes.Buffer
	for _, e := range sorted {
		payload := e.Vendor.NativePayload
		if len(payload) == 0 {
			// No verbatim line; try to synthesize a native rollout line
			// for the kinds that have a faithful form.
			synth := synthesizeRolloutLine(e)
			if synth == nil {
				continue
			}
			payload = synth
		}
		buf.Write(payload)
		if payload[len(payload)-1] != '\n' {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes(), nil
}

// synthesizeRolloutLine builds a best-effort rollout JSONL line for an
// event that has no verbatim native payload (e.g. a hook-only session
// that was never reconciled). Returns nil for events with no
// synthesizable native form (they're dropped from replay output).
//
// This is the documented happy path: session_start → session_meta,
// user/assistant messages → message response_items, thinking → reasoning
// response_items. Tool-call sub-shapes without a native payload are NOT
// synthesized here (open item #2) and are dropped.
func synthesizeRolloutLine(e event.Event) json.RawMessage {
	ts := e.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	switch e.Kind {
	case event.KindSessionStart:
		var c struct {
			Cwd        string `json:"cwd"`
			GitBranch  string `json:"git_branch"`
			CLIVersion string `json:"cli_version"`
			Source     string `json:"source"`
		}
		_ = json.Unmarshal(e.Content, &c)
		payload := map[string]any{
			"id":          e.SessionID,
			"timestamp":   ts,
			"cwd":         c.Cwd,
			"cli_version": c.CLIVersion,
			"originator":  "resleeve",
			"source":      "cli",
		}
		if c.GitBranch != "" {
			payload["git"] = map[string]any{"branch": c.GitBranch}
		}
		return marshalRolloutLine(ts, "session_meta", payload)
	case event.KindUserMessage, event.KindAssistantMessage:
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Content, &c)
		role := "user"
		if e.Kind == event.KindAssistantMessage {
			role = "assistant"
		}
		ctype := "input_text"
		if role == "assistant" {
			ctype = "output_text"
		}
		payload := map[string]any{
			"type":    "message",
			"role":    role,
			"content": []any{map[string]any{"type": ctype, "text": c.Text}},
		}
		return marshalRolloutLine(ts, "response_item", payload)
	case event.KindThinking:
		// Mirror fromnative's reasoning ingestion: a thinking event maps
		// to a reasoning response_item whose summary[].text carries the
		// reasoning text. Without this case a thinking event lacking a
		// verbatim native payload (e.g. cross-CLI capture) would be
		// silently dropped from replay. reasoningText() reads exactly the
		// summary[].text shape we emit here, so the round-trip is stable.
		var c struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Content, &c)
		payload := map[string]any{
			"type": "reasoning",
			"summary": []any{
				map[string]any{"type": "summary_text", "text": c.Text},
			},
		}
		return marshalRolloutLine(ts, "response_item", payload)
	default:
		return nil
	}
}

func marshalRolloutLine(ts, typ string, payload map[string]any) json.RawMessage {
	line := map[string]any{
		"timestamp": ts,
		"type":      typ,
		"payload":   payload,
	}
	b, _ := json.Marshal(line)
	return b
}
