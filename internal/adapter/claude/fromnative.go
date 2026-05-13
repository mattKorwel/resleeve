package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// hookEnvelope is the JSON Claude Code passes on stdin to a hook command.
// Fields are a superset across hook event types; unused fields are
// omitempty.
type hookEnvelope struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	HookEventName  string `json:"hook_event_name"`
	Cwd            string `json:"cwd,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	Source         string `json:"source,omitempty"` // SessionStart: startup|resume

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

// FromNative translates a native input — one hook envelope OR one
// JSONL line — into one or more normalized events.
func (a *Adapter) FromNative(ctx context.Context, raw []byte, src adapter.Source) ([]event.Event, error) {
	switch src.Kind {
	case adapter.SourceHook:
		return a.fromHook(raw)
	case adapter.SourceJSONL:
		return a.fromJSONL(raw)
	default:
		return nil, fmt.Errorf("claude: unknown source kind %v", src.Kind)
	}
}

// --- hook path ---

func (a *Adapter) fromHook(raw []byte) ([]event.Event, error) {
	var env hookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode hook envelope: %w", err)
	}
	if env.SessionID == "" {
		return nil, errors.New("claude: hook envelope missing session_id")
	}

	now := time.Now().UTC()
	slot := event.Slot{Scope: deriveScope(env.Cwd), AgentName: "default"}
	vendor := event.Vendor{Name: Name, Version: "", NativePayload: json.RawMessage(raw)}
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
			return nil, nil
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

// --- JSONL path ---

func (a *Adapter) fromJSONL(line []byte) ([]event.Event, error) {
	rec, err := ParseRecord(line)
	if err != nil {
		return nil, fmt.Errorf("parse jsonl: %w", err)
	}
	if rec.SessionID == "" || rec.Type == "" {
		return nil, nil
	}

	ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	slot := event.Slot{Scope: deriveScope(rec.Cwd), AgentName: "default"}
	vendor := event.Vendor{
		Name:          Name,
		Version:       rec.Version,
		NativePayload: append(json.RawMessage(nil), line...),
	}
	base := event.Event{
		SessionID:     rec.SessionID,
		Slot:          slot,
		Timestamp:     ts,
		SchemaVersion: 1,
		Vendor:        vendor,
	}
	if rec.ParentUUID != nil && *rec.ParentUUID != "" {
		p := *rec.ParentUUID
		base.ParentEventUUID = &p
	}

	switch rec.Type {
	case "user":
		return a.userFromJSONL(rec, base)
	case "assistant":
		return a.assistantFromJSONL(rec, base)
	case "attachment":
		return a.systemFromJSONL(rec, base, "attachment", event.KindAttachment), nil
	case "permission-mode":
		return a.systemFromJSONL(rec, base, "permission_mode", event.KindSystem), nil
	case "file-history-snapshot":
		return a.systemFromJSONL(rec, base, "file_history_snapshot", event.KindSystem), nil
	case "system", "ai-title", "last-prompt":
		return a.systemFromJSONL(rec, base, rec.Type, event.KindSystem), nil
	default:
		return a.systemFromJSONL(rec, base, "unknown:"+rec.Type, event.KindSystem), nil
	}
}

func (a *Adapter) userFromJSONL(rec *Record, base event.Event) ([]event.Event, error) {
	if len(rec.Message) == 0 {
		return nil, nil
	}
	var msg userMessage
	if err := json.Unmarshal(rec.Message, &msg); err != nil {
		return nil, fmt.Errorf("decode user message: %w", err)
	}

	// Content can be a JSON string or an array of blocks.
	if isJSONString(msg.Content) {
		var text string
		if err := json.Unmarshal(msg.Content, &text); err != nil {
			return nil, fmt.Errorf("decode user content string: %w", err)
		}
		key := rec.UUID
		if rec.PromptID != "" {
			key = "user:" + rec.PromptID
		}
		content := mustJSON(map[string]any{
			"text":          text,
			"gen_ai.system": "claude_code",
		})
		e := base
		e.EventUUID = DeterministicEventUUID(rec.SessionID, key)
		e.Kind = event.KindUserMessage
		e.Seq = base.Timestamp.UnixNano()
		e.Content = content
		return []event.Event{e}, nil
	}

	var blocks []claudeContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, fmt.Errorf("decode user content blocks: %w", err)
	}
	var out []event.Event
	for i, b := range blocks {
		e := base
		switch b.Type {
		case "tool_result":
			status := "success"
			if b.IsError {
				status = "error"
			}
			content := mustJSON(map[string]any{
				"tool": map[string]any{
					"call_id": b.ToolUseID,
					"status":  status,
					"result":  b.Content,
				},
			})
			e.EventUUID = DeterministicEventUUID(rec.SessionID, "result:"+b.ToolUseID)
			e.Kind = event.KindToolResult
			e.Seq = base.Timestamp.UnixNano() + int64(i)
			e.Content = content
		case "text":
			content := mustJSON(map[string]any{
				"text":          b.Text,
				"gen_ai.system": "claude_code",
			})
			e.EventUUID = DeterministicEventUUID(rec.SessionID, rec.UUID+":text:"+strconv.Itoa(i))
			e.Kind = event.KindUserMessage
			e.Seq = base.Timestamp.UnixNano() + int64(i)
			e.Content = content
		default:
			content := mustJSON(map[string]any{
				"block_type":           b.Type,
				"resleeve.system.kind": "user_block:" + b.Type,
			})
			e.EventUUID = DeterministicEventUUID(rec.SessionID, rec.UUID+":"+b.Type+":"+strconv.Itoa(i))
			e.Kind = event.KindSystem
			e.Seq = base.Timestamp.UnixNano() + int64(i)
			e.Content = content
		}
		out = append(out, e)
	}
	return out, nil
}

func (a *Adapter) assistantFromJSONL(rec *Record, base event.Event) ([]event.Event, error) {
	if len(rec.Message) == 0 {
		return nil, nil
	}
	var msg assistantMessage
	if err := json.Unmarshal(rec.Message, &msg); err != nil {
		return nil, fmt.Errorf("decode assistant message: %w", err)
	}

	turnID := rec.UUID
	var out []event.Event
	for i, b := range msg.Content {
		e := base
		e.TurnID = &turnID

		switch b.Type {
		case "text":
			content := mustJSON(map[string]any{
				"text":                  b.Text,
				"gen_ai.request.model":  msg.Model,
				"gen_ai.response.model": msg.Model,
				"gen_ai.system":         "claude_code",
				"stop_reason":           msg.StopReason,
			})
			e.EventUUID = DeterministicEventUUID(rec.SessionID, rec.UUID+":text:"+strconv.Itoa(i))
			e.Kind = event.KindAssistantMessage
			e.Seq = base.Timestamp.UnixNano() + int64(i)
			e.Content = content
		case "tool_use":
			content := mustJSON(map[string]any{
				"tool": map[string]any{
					"id":   b.ID,
					"name": b.Name,
					"args": b.Input,
				},
				"gen_ai.request.model": msg.Model,
			})
			e.EventUUID = DeterministicEventUUID(rec.SessionID, "call:"+b.ID)
			e.Kind = event.KindToolCall
			e.Seq = base.Timestamp.UnixNano() + int64(i)
			e.Content = content
		case "thinking":
			content := mustJSON(map[string]any{
				"text":                 b.Thinking,
				"resleeve.thinking":    true,
				"gen_ai.request.model": msg.Model,
			})
			e.EventUUID = DeterministicEventUUID(rec.SessionID, rec.UUID+":thinking:"+strconv.Itoa(i))
			e.Kind = event.KindThinking
			e.Seq = base.Timestamp.UnixNano() + int64(i)
			e.Content = content
		default:
			content := mustJSON(map[string]any{
				"block_type":           b.Type,
				"resleeve.system.kind": "assistant_block:" + b.Type,
			})
			e.EventUUID = DeterministicEventUUID(rec.SessionID, rec.UUID+":"+b.Type+":"+strconv.Itoa(i))
			e.Kind = event.KindSystem
			e.Seq = base.Timestamp.UnixNano() + int64(i)
			e.Content = content
		}
		out = append(out, e)
	}
	return out, nil
}

// systemFromJSONL emits a single event for a JSONL record we don't
// decompose further. The raw record is preserved in vendor.native_payload.
func (a *Adapter) systemFromJSONL(rec *Record, base event.Event, kind string, eventKind event.Kind) []event.Event {
	content := mustJSON(map[string]any{
		"resleeve.system.kind": kind,
	})
	e := base
	e.EventUUID = DeterministicEventUUID(rec.SessionID, rec.UUID)
	e.Kind = eventKind
	e.Seq = base.Timestamp.UnixNano()
	e.Content = content
	return []event.Event{e}
}

// --- helpers ---

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

// isJSONString reports whether the JSON bytes encode a string (begin with ").
func isJSONString(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '"':
			return true
		default:
			return false
		}
	}
	return false
}
