package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// hookEnvelope is the JSON Claude Code passes on stdin to a hook command.
// Fields are a superset across hook event types; unused fields are
// omitempty.
type hookEnvelope struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	HookEventName  string          `json:"hook_event_name"`
	Cwd            string          `json:"cwd,omitempty"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	Source         string          `json:"source,omitempty"` // SessionStart: startup|resume

	// PostToolUse
	ToolName     string          `json:"tool_name,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	ToolInput    json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse json.RawMessage `json:"tool_response,omitempty"`
	ToolError    string          `json:"tool_error,omitempty"`

	// UserPromptSubmit
	PromptID string `json:"prompt_id,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
}

// FromNative translates a native input (one hook envelope, or one
// JSONL line) into one or more normalized events. v1 implements the
// hook path; the JSONL path lands in Stage 3c alongside the reconcile
// sweep.
func (a *Adapter) FromNative(ctx context.Context, raw []byte, src adapter.Source) ([]event.Event, error) {
	switch src.Kind {
	case adapter.SourceHook:
		return a.fromHook(raw)
	case adapter.SourceJSONL:
		return nil, fmt.Errorf("%w: claude JSONL parsing lands in Stage 3c", adapter.ErrNotImplemented)
	default:
		return nil, fmt.Errorf("claude: unknown source kind %v", src.Kind)
	}
}

func (a *Adapter) fromHook(raw []byte) ([]event.Event, error) {
	var env hookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode hook envelope: %w", err)
	}
	if env.SessionID == "" {
		return nil, errors.New("claude: hook envelope missing session_id")
	}

	now := time.Now().UTC()
	slot := event.Slot{
		Scope:     deriveScope(env.Cwd),
		AgentName: "default",
	}
	vendor := event.Vendor{
		Name:          Name,
		Version:       "", // hook envelope doesn't carry CLI version; reconcile backfills
		NativePayload: json.RawMessage(raw),
	}
	base := event.Event{
		SessionID:     env.SessionID,
		Slot:          slot,
		Timestamp:     now,
		SchemaVersion: 1,
		Vendor:        vendor,
	}

	switch env.HookEventName {
	case "SessionStart":
		content := mustJSON(map[string]any{
			"cli":             Name,
			"cwd":             env.Cwd,
			"transcript_path": env.TranscriptPath,
			"source":          env.Source,
			"permission_mode": env.PermissionMode,
		})
		e := base
		e.EventUUID = DeterministicEventUUID(env.SessionID, "session_start")
		e.Kind = event.KindSessionStart
		e.Seq = now.UnixNano()
		e.Content = content
		return []event.Event{e}, nil

	case "Stop":
		content := mustJSON(map[string]any{
			"ended_at": now.Format(time.RFC3339Nano),
			"reason":   "normal",
		})
		e := base
		e.EventUUID = DeterministicEventUUID(env.SessionID, "session_end")
		e.Kind = event.KindSessionEnd
		e.Seq = now.UnixNano()
		e.Content = content
		return []event.Event{e}, nil

	case "PostToolUse":
		if env.ToolUseID == "" {
			return nil, nil // skip silently
		}
		callContent := mustJSON(map[string]any{
			"tool": map[string]any{
				"id":   env.ToolUseID,
				"name": env.ToolName,
				"args": env.ToolInput,
			},
		})
		status := "success"
		if env.ToolError != "" {
			status = "error"
		}
		resultContent := mustJSON(map[string]any{
			"tool": map[string]any{
				"call_id": env.ToolUseID,
				"status":  status,
				"result":  env.ToolResponse,
			},
		})

		call := base
		call.EventUUID = DeterministicEventUUID(env.SessionID, "call:"+env.ToolUseID)
		call.Kind = event.KindToolCall
		call.Seq = now.UnixNano()
		call.Content = callContent

		result := base
		result.EventUUID = DeterministicEventUUID(env.SessionID, "result:"+env.ToolUseID)
		result.Kind = event.KindToolResult
		result.Seq = now.UnixNano() + 1
		result.Content = resultContent

		return []event.Event{call, result}, nil

	case "UserPromptSubmit":
		if env.PromptID == "" {
			return nil, nil
		}
		content := mustJSON(map[string]any{
			"text":          env.Prompt,
			"gen_ai.system": "claude_code",
		})
		e := base
		e.EventUUID = DeterministicEventUUID(env.SessionID, "user:"+env.PromptID)
		e.Kind = event.KindUserMessage
		e.Seq = now.UnixNano()
		e.Content = content
		return []event.Event{e}, nil

	default:
		// Unknown hook event — preserve as a generic system marker.
		content := mustJSON(map[string]any{
			"hook_event_name": env.HookEventName,
			"raw_hook":        json.RawMessage(raw),
		})
		e := base
		e.EventUUID = DeterministicEventUUID(env.SessionID, "hook:"+env.HookEventName)
		e.Kind = event.KindSystem
		e.Seq = now.UnixNano()
		e.Content = content
		return []event.Event{e}, nil
	}
}

func deriveScope(cwd string) string {
	if cwd == "" {
		return "unknown"
	}
	return filepath.Base(cwd)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
