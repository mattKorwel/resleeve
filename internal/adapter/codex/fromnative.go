package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// userMessageKey returns the deterministic dedup key for a user prompt.
//
// Codex gives us no stable id we can share across capture paths: the
// UserPromptSubmit hook carries a turn_id but the rollout does not surface
// that turn_id on either user-message form (the event_msg user_message
// payload has only client_id, and the response_item message(role=user) has
// no id). The one field common to the hook and the canonical rollout form
// is the prompt TEXT itself, so we key on a hash of it. This lets the live
// hook capture and the reconcile pass compute the SAME (session_id, key)
// and dedup via INSERT OR IGNORE.
func userMessageKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "user:" + hex.EncodeToString(sum[:])
}

// ScopeMarkerFile is the filename resleeve looks for when walking cwd
// ancestors to resolve a hierarchical scope. See deriveScope.
const ScopeMarkerFile = ".resleeve-scope"

// hookEnvelope is the JSON Codex passes on stdin to a hook command. It is
// a superset across the lifecycle events resleeve cares about; unused
// fields per event stay zero. Verified hooks/src/schema.rs (snake_case
// field names). transcript_path is nullable in Codex; we accept a string
// or null via a json.RawMessage probe.
type hookEnvelope struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath json.RawMessage `json:"transcript_path,omitempty"`
	HookEventName  string          `json:"hook_event_name"`
	Cwd            string          `json:"cwd,omitempty"`
	Model          string          `json:"model,omitempty"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	Source         string          `json:"source,omitempty"` // SessionStart: startup|resume|clear|compact
	TurnID         string          `json:"turn_id,omitempty"`

	// PostToolUse
	ToolName     string          `json:"tool_name,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	ToolInput    json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse json.RawMessage `json:"tool_response,omitempty"`

	// UserPromptSubmit
	Prompt string `json:"prompt,omitempty"`

	// Stop
	StopHookActive       bool            `json:"stop_hook_active,omitempty"`
	LastAssistantMessage json.RawMessage `json:"last_assistant_message,omitempty"`
}

// transcriptPath returns the transcript_path as a string, tolerating the
// nullable encoding (Codex wraps it as a nullable string).
func (e *hookEnvelope) transcriptPath() string {
	if len(e.TranscriptPath) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(e.TranscriptPath, &s); err == nil {
		return s
	}
	return ""
}

func (e *hookEnvelope) lastAssistantMessage() string {
	if len(e.LastAssistantMessage) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(e.LastAssistantMessage, &s); err == nil {
		return s
	}
	return ""
}

// FromNative translates a native input — one Codex hook envelope OR one
// rollout JSONL line — into one or more normalized events.
func (a *Adapter) FromNative(ctx context.Context, raw []byte, src adapter.Source) ([]event.Event, error) {
	switch src.Kind {
	case adapter.SourceHook:
		return a.fromHook(raw)
	case adapter.SourceJSONL:
		return a.fromJSONL(raw, src.SessionHint)
	default:
		return nil, fmt.Errorf("codex: unknown source kind %v", src.Kind)
	}
}

// --- hook path ---

func (a *Adapter) fromHook(raw []byte) ([]event.Event, error) {
	var env hookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode hook envelope: %w", err)
	}
	if env.SessionID == "" {
		return nil, errors.New("codex: hook envelope missing session_id")
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
	if env.TurnID != "" {
		t := env.TurnID
		base.TurnID = &t
	}

	switch env.HookEventName {
	case "SessionStart":
		content := mustJSON(map[string]any{
			"cli":             Name,
			"cwd":             env.Cwd,
			"model":           env.Model,
			"transcript_path": env.transcriptPath(),
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
			"ended_at":               now.Format(time.RFC3339Nano),
			"reason":                 "normal",
			"last_assistant_message": env.lastAssistantMessage(),
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
				"args": rawOrNull(env.ToolInput),
			},
			"gen_ai.system": "codex",
		})
		resultContent := mustJSON(map[string]any{
			"tool": map[string]any{
				"call_id": env.ToolUseID,
				"status":  "success",
				"result":  rawOrNull(env.ToolResponse),
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
		// Key on the prompt TEXT (not turn_id): the rollout's canonical
		// user-message form (event_msg user_message) does not surface a
		// turn_id, so a content hash is the only key shared across the
		// live hook and the reconcile pass. See userMessageKey.
		content := mustJSON(map[string]any{
			"text":          env.Prompt,
			"gen_ai.system": "codex",
		})
		e := base
		e.EventUUID = DeterministicEventUUID(env.SessionID, userMessageKey(env.Prompt))
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

// --- rollout JSONL path ---

func (a *Adapter) fromJSONL(line []byte, sessionHint string) ([]event.Event, error) {
	rl, err := ParseRolloutLine(line)
	if err != nil {
		return nil, fmt.Errorf("parse rollout line: %w", err)
	}
	if rl.Type == "" {
		return nil, nil
	}

	ts, _ := time.Parse(time.RFC3339Nano, rl.Timestamp)
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	switch rl.Type {
	case "session_meta":
		return a.sessionMetaFromJSONL(rl, line, ts)
	case "turn_context":
		return a.turnContextFromJSONL(rl, line, ts, sessionHint)
	case "response_item":
		return a.responseItemFromJSONL(rl, line, ts, sessionHint)
	case "event_msg":
		return a.eventMsgFromJSONL(rl, line, ts, sessionHint)
	case "compacted":
		return a.compactedFromJSONL(rl, line, ts, sessionHint)
	default:
		// Unknown line type: record as system so nothing is silently lost.
		if sessionHint == "" {
			return nil, nil
		}
		base := baseEvent(sessionHint, "", ts, line)
		base.EventUUID = DeterministicEventUUID(sessionHint, "line:"+rl.Type+":"+rl.Timestamp)
		base.Kind = event.KindSystem
		base.Seq = ts.UnixNano()
		base.Content = mustJSON(map[string]any{"resleeve.system.kind": "unknown:" + rl.Type})
		return []event.Event{base}, nil
	}
}

func (a *Adapter) sessionMetaFromJSONL(rl *RolloutLine, line []byte, ts time.Time) ([]event.Event, error) {
	var meta SessionMetaPayload
	if err := json.Unmarshal(rl.Payload, &meta); err != nil {
		return nil, fmt.Errorf("decode session_meta: %w", err)
	}
	if meta.ID == "" {
		return nil, nil
	}
	branch := ""
	if meta.Git != nil {
		branch = meta.Git.Branch
	}
	content := mustJSON(map[string]any{
		"cli":         Name,
		"cwd":         meta.Cwd,
		"git_branch":  branch,
		"cli_version": meta.CLIVersion,
		"source":      meta.Source,
	})
	base := baseEvent(meta.ID, meta.Cwd, ts, line)
	base.Vendor.Version = meta.CLIVersion
	base.EventUUID = DeterministicEventUUID(meta.ID, "session_start")
	base.Kind = event.KindSessionStart
	base.Seq = ts.UnixNano()
	base.Content = content
	return []event.Event{base}, nil
}

func (a *Adapter) turnContextFromJSONL(rl *RolloutLine, line []byte, ts time.Time, sessionHint string) ([]event.Event, error) {
	var tc TurnContextPayload
	if err := json.Unmarshal(rl.Payload, &tc); err != nil {
		return nil, fmt.Errorf("decode turn_context: %w", err)
	}
	sid := sessionHint
	if sid == "" {
		return nil, nil
	}
	cwd := tc.Cwd
	content := mustJSON(map[string]any{
		"resleeve.system.kind": "turn_context",
		"gen_ai.request.model": tc.Model,
		"model":                tc.Model,
		"approval_policy":      tc.ApprovalPolicy,
	})
	base := baseEvent(sid, cwd, ts, line)
	key := "turn_context:" + tc.TurnID
	if tc.TurnID == "" {
		key = "turn_context:" + rl.Timestamp
	}
	base.EventUUID = DeterministicEventUUID(sid, key)
	base.Kind = event.KindSystem
	base.Seq = ts.UnixNano()
	base.Content = content
	if tc.TurnID != "" {
		t := tc.TurnID
		base.TurnID = &t
	}
	return []event.Event{base}, nil
}

func (a *Adapter) responseItemFromJSONL(rl *RolloutLine, line []byte, ts time.Time, sessionHint string) ([]event.Event, error) {
	var item ResponseItemPayload
	if err := json.Unmarshal(rl.Payload, &item); err != nil {
		return nil, fmt.Errorf("decode response_item: %w", err)
	}
	sid := sessionHint
	if sid == "" {
		return nil, nil
	}
	base := baseEvent(sid, "", ts, line)

	switch item.Type {
	case "message":
		return a.messageFromResponseItem(&item, rl, base, sid, ts), nil
	case "function_call", "custom_tool_call", "local_shell_call":
		key := "call:" + item.CallID
		if item.CallID == "" {
			key = "call:" + rl.Timestamp
		}
		args := item.Arguments
		if len(args) == 0 {
			args = item.Input
		}
		content := mustJSON(map[string]any{
			"tool": map[string]any{
				"id":   item.CallID,
				"name": item.Name,
				"args": rawOrNull(args),
			},
			"gen_ai.system": "codex",
		})
		base.EventUUID = DeterministicEventUUID(sid, key)
		base.Kind = event.KindToolCall
		base.Seq = ts.UnixNano()
		base.Content = content
		return []event.Event{base}, nil
	case "function_call_output", "custom_tool_call_output":
		key := "result:" + item.CallID
		if item.CallID == "" {
			key = "result:" + rl.Timestamp
		}
		content := mustJSON(map[string]any{
			"tool": map[string]any{
				"call_id": item.CallID,
				"status":  "success",
				"result":  rawOrNull(item.Output),
			},
		})
		base.EventUUID = DeterministicEventUUID(sid, key)
		base.Kind = event.KindToolResult
		base.Seq = ts.UnixNano()
		base.Content = content
		return []event.Event{base}, nil
	case "reasoning":
		content := mustJSON(map[string]any{
			"text":              reasoningText(rl.Payload),
			"resleeve.thinking": true,
		})
		base.EventUUID = DeterministicEventUUID(sid, "reasoning:"+responseItemKey(&item, rl))
		base.Kind = event.KindThinking
		base.Seq = ts.UnixNano()
		base.Content = content
		return []event.Event{base}, nil
	default:
		base.EventUUID = DeterministicEventUUID(sid, "item:"+item.Type+":"+responseItemKey(&item, rl))
		base.Kind = event.KindSystem
		base.Seq = ts.UnixNano()
		base.Content = mustJSON(map[string]any{"resleeve.system.kind": "response_item:" + item.Type})
		return []event.Event{base}, nil
	}
}

// messageFromResponseItem turns a Message response_item into an assistant
// message (keyed by role) or, for role=user, a SYSTEM record.
//
// IMPORTANT (user-message dedup): Codex writes a user turn TWICE in the
// rollout — once as an event_msg user_message (the clean prompt text) and
// once as a response_item message(role=user). The response_item form is a
// superset: it also carries injected context the model sees but the user
// never typed (AGENTS.md instructions, <environment_context>,
// <turn_aborted>, file-mention preambles). Verified against live rollouts
// under ~/.codex/sessions. To avoid double-ingesting one logical prompt we
// pick event_msg user_message as the SINGLE canonical KindUserMessage
// source and record the response_item user form as KindSystem — preserved
// (verbatim native payload + text) but not a duplicate user message.
func (a *Adapter) messageFromResponseItem(item *ResponseItemPayload, rl *RolloutLine, base event.Event, sid string, ts time.Time) []event.Event {
	text := joinContentText(item.Content)
	key := responseItemKey(item, rl)
	switch item.Role {
	case "user":
		base.EventUUID = DeterministicEventUUID(sid, "msg:user:"+key)
		base.Kind = event.KindSystem
		base.Seq = ts.UnixNano()
		base.Content = mustJSON(map[string]any{
			"resleeve.system.kind": "message:user",
			"text":                 text,
		})
	case "assistant":
		base.EventUUID = DeterministicEventUUID(sid, "assistant:"+key)
		base.Kind = event.KindAssistantMessage
		base.Seq = ts.UnixNano()
		base.Content = mustJSON(map[string]any{"text": text, "gen_ai.system": "codex"})
	default:
		base.EventUUID = DeterministicEventUUID(sid, "msg:"+item.Role+":"+key)
		base.Kind = event.KindSystem
		base.Seq = ts.UnixNano()
		base.Content = mustJSON(map[string]any{
			"resleeve.system.kind": "message:" + item.Role,
			"text":                 text,
		})
	}
	return []event.Event{base}
}

func (a *Adapter) eventMsgFromJSONL(rl *RolloutLine, line []byte, ts time.Time, sessionHint string) ([]event.Event, error) {
	var ev EventMsgPayload
	if err := json.Unmarshal(rl.Payload, &ev); err != nil {
		return nil, fmt.Errorf("decode event_msg: %w", err)
	}
	sid := sessionHint
	if sid == "" {
		return nil, nil
	}
	// Only `user_message` carries durable content; drop other UI events.
	if ev.Type != "user_message" {
		return nil, nil
	}
	base := baseEvent(sid, "", ts, line)
	// event_msg user_message is the canonical user-prompt source. Key on
	// the prompt text so the live UserPromptSubmit hook capture and this
	// reconcile form compute the SAME deterministic UUID and dedup. See
	// userMessageKey.
	base.EventUUID = DeterministicEventUUID(sid, userMessageKey(ev.Message))
	base.Kind = event.KindUserMessage
	base.Seq = ts.UnixNano()
	base.Content = mustJSON(map[string]any{"text": ev.Message, "gen_ai.system": "codex"})
	return []event.Event{base}, nil
}

func (a *Adapter) compactedFromJSONL(rl *RolloutLine, line []byte, ts time.Time, sessionHint string) ([]event.Event, error) {
	sid := sessionHint
	if sid == "" {
		return nil, nil
	}
	var c CompactedPayload
	_ = json.Unmarshal(rl.Payload, &c)
	base := baseEvent(sid, "", ts, line)
	base.EventUUID = DeterministicEventUUID(sid, "compacted:"+rl.Timestamp)
	base.Kind = event.KindSystem
	base.Seq = ts.UnixNano()
	base.Content = mustJSON(map[string]any{
		"resleeve.system.kind": "compacted",
		"message":              c.Message,
	})
	return []event.Event{base}, nil
}

// synthesizeSessionStart builds a synthetic session_start for a session
// observed only via hook (never reconciled) or whose first rollout line
// is not a session_meta. Uses the same deterministic key as the real
// session_meta path so live + reconcile dedup cleanly.
func synthesizeSessionStart(sessionID, cwd, version string, ts time.Time) event.Event {
	content := mustJSON(map[string]any{
		"cli":    Name,
		"cwd":    cwd,
		"source": "reconcile",
	})
	return event.Event{
		EventUUID:     DeterministicEventUUID(sessionID, "session_start"),
		SessionID:     sessionID,
		Slot:          event.Slot{Scope: deriveScope(cwd), AgentName: "default"},
		Seq:           ts.UnixNano() - 1,
		Timestamp:     ts,
		Kind:          event.KindSessionStart,
		SchemaVersion: 1,
		Content:       content,
		Vendor:        event.Vendor{Name: Name, Version: version},
	}
}

// --- helpers ---

func baseEvent(sessionID, cwd string, ts time.Time, line []byte) event.Event {
	return event.Event{
		SessionID:     sessionID,
		Slot:          event.Slot{Scope: deriveScope(cwd), AgentName: "default"},
		Timestamp:     ts,
		SchemaVersion: 1,
		Vendor: event.Vendor{
			Name:          Name,
			NativePayload: append(json.RawMessage(nil), line...),
		},
	}
}

// responseItemKey returns a stable key for a response_item that has no
// natural id. Prefers the item id, then the call_id, then a timestamp
// + role discriminator so distinct items on the same line key apart.
func responseItemKey(item *ResponseItemPayload, rl *RolloutLine) string {
	if item.ID != "" {
		return item.ID
	}
	if item.CallID != "" {
		return item.CallID
	}
	return rl.Timestamp + ":" + item.Role
}

// joinContentText concatenates the text of input_text/output_text content
// blocks, separated by newlines. Image blocks contribute a placeholder.
func joinContentText(items []ResponseContentItem) string {
	var parts []string
	for _, c := range items {
		switch c.Type {
		case "input_text", "output_text", "text":
			parts = append(parts, c.Text)
		case "input_image", "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

// reasoningText pulls a best-effort text out of a reasoning response_item
// payload (summary[].text). Returns "" when none is decodable.
func reasoningText(payload json.RawMessage) string {
	var r struct {
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(payload, &r); err != nil {
		return ""
	}
	var parts []string
	for _, s := range r.Summary {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// rawOrNull returns the raw JSON if non-empty, else a JSON null literal so
// the embedding map serializes a stable shape.
func rawOrNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`null`)
	}
	return raw
}

// DeriveScope is the exported entry point for the scope-from-cwd resolver.
func DeriveScope(cwd string) string { return deriveScope(cwd) }

// deriveScope picks a scope path for an event given its cwd. Resolution
// order mirrors the claude adapter:
//  1. $RESLEEVE_SCOPE — verbatim override.
//  2. Walk cwd ancestors for a `.resleeve-scope` marker file.
//  3. filepath.Base(cwd), or "unknown" if cwd is empty.
func deriveScope(cwd string) string {
	if env := strings.TrimSpace(os.Getenv("RESLEEVE_SCOPE")); env != "" {
		return env
	}
	if cwd == "" {
		return "unknown"
	}
	if marker, dir := findScopeMarker(cwd); marker != "" {
		rel, err := filepath.Rel(dir, cwd)
		if err != nil || rel == "." {
			return marker
		}
		return marker + "/" + filepath.ToSlash(rel)
	}
	return filepath.Base(cwd)
}

func findScopeMarker(cwd string) (marker, markerDir string) {
	dir := cwd
	for {
		if b, err := os.ReadFile(filepath.Join(dir, ScopeMarkerFile)); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v, dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
