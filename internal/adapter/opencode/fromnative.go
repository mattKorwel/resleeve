package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// ScopeMarkerFile is the filename resleeve looks for when walking cwd
// ancestors to resolve a hierarchical scope. See deriveScope.
const ScopeMarkerFile = ".resleeve-scope"

// FromNative translates a native opencode input into normalized events.
//
// opencode capture is bridge-free, so the only live input shape is one
// GET /event SSE frame (the JSON object after `data:`). Pass it with
// Source{Kind: SourceHook} — the daemon's SSE subscriber (sse.go) does
// exactly this. Backfill goes through ReconcileOnce → rowsToEvents
// instead, not FromNative.
func (a *Adapter) FromNative(ctx context.Context, raw []byte, src adapter.Source) ([]event.Event, error) {
	switch src.Kind {
	case adapter.SourceHook:
		return a.fromSSEFrame(raw, src.SessionHint)
	default:
		return nil, fmt.Errorf("opencode: unsupported source kind %v (opencode capture is bridge-free: SSE frames via SourceHook, or ReconcileOnce for db backfill)", src.Kind)
	}
}

// --- shared row → event mapping (db reader + import round-trip) ---

// sessionStartEvent builds the KindSessionStart event for a session row.
// directory is on the session (verified session/sql.ts:32), so cwd/scope
// come straight off the row.
func sessionStartEvent(s sessionRow, rawRow json.RawMessage) event.Event {
	ts := msToTime(s.TimeCreated)
	model := modelString(s.Model)
	content := mustJSON(map[string]any{
		"cli":   Name,
		"cwd":   s.Directory,
		"model": model,
		"title": s.Title,
	})
	return event.Event{
		EventUUID:     DeterministicEventUUID(s.ID, "session_start"),
		SessionID:     s.ID,
		Slot:          event.Slot{Scope: deriveScope(s.Directory), AgentName: "default"},
		Seq:           ts.UnixNano() - 1, // sort just before the first message
		Timestamp:     ts,
		Kind:          event.KindSessionStart,
		SchemaVersion: 1,
		Content:       content,
		Vendor:        vendorFor(s.Version, rawRow),
	}
}

// messageEvents converts a message row + its parts into normalized
// events. The session's directory (scope) and version are threaded in so
// every event carries consistent slot/vendor metadata.
//
// opencode stores Message.data / Part.data with id / sessionID / messageID
// HOISTED to their own columns (verified import.ts: it strips those keys
// before INSERT). So every native payload below is rebuilt by merging the
// hoisted column ids back into the data blob, yielding a complete
// SessionV1.Info / SessionV1.Part that validates on import round-trip.
//
// Text parts of one message are concatenated into a single user/assistant
// message event (matching the spec's "text parts concatenated"), and that
// message event also carries every text/reasoning part's full Part JSON in
// its native payload wrapper {info, parts} so the import round-trip can
// rebuild the message's complete Part[]. Tool parts each yield a
// KindToolCall + KindToolResult pair keyed on callID. Other part types fall
// through to a KindSystem event preserving the raw part in
// vendor.native_payload.
func messageEvents(m messageRow, parts []partRow, scopeDir, version string) []event.Event {
	var md messageData
	_ = json.Unmarshal(m.Data, &md)

	created := md.Time.Created
	if created == 0 {
		created = m.TimeCreated
	}
	baseTS := msToTime(created)
	slot := event.Slot{Scope: deriveScope(scopeDir), AgentName: "default"}

	// Re-attach the hoisted message ids so the payload is a full
	// SessionV1.Info (import expects id/sessionID present).
	msgPayload := mergeRowIDs(m.Data, map[string]string{"id": m.ID, "sessionID": m.SessionID})

	// Rank parts by their time-sortable prt_ ids so intra-message ordering
	// (text vs tool-call vs result) is deterministic and independent of the
	// slice's arrival order.
	ranked := make([]partRow, len(parts))
	copy(ranked, parts)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].ID < ranked[j].ID })
	rankOf := make(map[string]int, len(ranked))
	for i, p := range ranked {
		rankOf[p.ID] = i
	}

	var out []event.Event
	var textBuf []string
	var envParts []json.RawMessage // full Part JSON for every non-tool part (text, reasoning, structural)
	turnID := m.ID

	for _, p := range parts {
		var pd partData
		_ = json.Unmarshal(p.Data, &pd)
		// Offset by rank+1 so parts sort after the message event (rank 0
		// would collide with baseTS.UnixNano()) and never collide with each
		// other, regardless of arrival order.
		seq := baseTS.UnixNano() + int64(rankOf[p.ID]) + 1
		// Re-attach the hoisted part ids so each part is a full SessionV1.Part.
		partPayload := mergeRowIDs(p.Data, map[string]string{
			"id":        p.ID,
			"sessionID": m.SessionID,
			"messageID": m.ID,
		})

		switch pd.Type {
		case "text":
			if strings.TrimSpace(pd.Text) != "" {
				textBuf = append(textBuf, pd.Text)
			}
			envParts = append(envParts, partPayload)
		case "reasoning":
			// Reasoning is a Part, not message Info: append it to the
			// message's parts so import rebuilds it as a SessionV1.Part, and
			// also surface it on the normalized stream as a thinking event.
			envParts = append(envParts, partPayload)
			e := event.Event{
				EventUUID:     DeterministicEventUUID(m.SessionID, p.ID),
				SessionID:     m.SessionID,
				Slot:          slot,
				Seq:           seq,
				Timestamp:     baseTS,
				Kind:          event.KindThinking,
				SchemaVersion: 1,
				Content:       mustJSON(map[string]any{"text": pd.Text}),
				Vendor:        vendorFor(version, partPayload),
			}
			if md.Role == "assistant" {
				e.TurnID = &turnID
			}
			out = append(out, e)
		case "tool":
			out = append(out, toolEvents(m, p, pd, partPayload, slot, baseTS, seq, version, &turnID, md.Role)...)
		default:
			// Structural parts (step-start/step-finish, snapshot, patch, …):
			// carry the full Part JSON in the message envelope so the import
			// round-trip rebuilds them (they're valid SessionV1.Part types),
			// AND surface a KindSystem event on the normalized stream for
			// visibility. Without the envelope carry these would be dropped on
			// replay (the import path only re-emits envelope/tool parts).
			envParts = append(envParts, partPayload)
			e := event.Event{
				EventUUID:     DeterministicEventUUID(m.SessionID, p.ID),
				SessionID:     m.SessionID,
				Slot:          slot,
				Seq:           seq,
				Timestamp:     baseTS,
				Kind:          event.KindSystem,
				SchemaVersion: 1,
				Content:       mustJSON(map[string]any{"resleeve.system.kind": "part:" + fallbackStr(pd.Type, "unknown")}),
				Vendor:        vendorFor(version, partPayload),
			}
			out = append(out, e)
		}
	}

	// Emit the message envelope as a single event keyed on the message id so
	// the same message dedups across db/SSE. It is emitted whenever the
	// message has a known role (user/assistant) — even with no text (a
	// tool-only or reasoning-only turn) — so its info survives import and
	// carries its tool parts. Its native payload wraps the message info plus
	// the text/reasoning parts: {info, parts}.
	if md.Role == "user" || md.Role == "assistant" {
		kind := event.KindAssistantMessage
		if md.Role == "user" {
			kind = event.KindUserMessage
		}
		content := map[string]any{
			"text":          strings.Join(textBuf, ""),
			"gen_ai.system": "opencode",
		}
		if model := messageModel(md); model != "" {
			content["gen_ai.request.model"] = model
			content["gen_ai.response.model"] = model
		}
		e := event.Event{
			EventUUID:     DeterministicEventUUID(m.SessionID, m.ID),
			SessionID:     m.SessionID,
			Slot:          slot,
			Seq:           baseTS.UnixNano(),
			Timestamp:     baseTS,
			Kind:          kind,
			SchemaVersion: 1,
			Content:       mustJSON(content),
			Vendor:        vendorFor(version, wrapMessagePayload(msgPayload, envParts)),
		}
		if kind == event.KindAssistantMessage {
			e.TurnID = &turnID
		}
		out = append(out, e)
	}

	return out
}

// messageEnvelope is the native-payload wrapper a message event carries:
// the full SessionV1.Info plus the message's text/reasoning Part JSON, so
// the import round-trip can rebuild both the message info and its parts.
// Tool parts are carried by their own KindToolCall events.
type messageEnvelope struct {
	Info  json.RawMessage   `json:"info"`
	Parts []json.RawMessage `json:"parts,omitempty"`
}

// wrapMessagePayload bundles a message info with its text/reasoning parts.
func wrapMessagePayload(info json.RawMessage, parts []json.RawMessage) json.RawMessage {
	return mustJSON(messageEnvelope{Info: info, Parts: parts})
}

// mergeRowIDs returns data with the given id keys ensured present. opencode
// hoists id/sessionID/messageID out of the stored data blob into their own
// columns; this re-attaches them so the payload is a complete SessionV1
// object. Existing non-empty values in data win (SSE frames already carry
// full blobs); only missing/empty keys are filled from the columns.
func mergeRowIDs(data json.RawMessage, ids map[string]string) json.RawMessage {
	obj := map[string]json.RawMessage{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &obj)
	}
	for k, v := range ids {
		if v == "" {
			continue
		}
		if existing, ok := obj[k]; ok {
			var s string
			if json.Unmarshal(existing, &s) == nil && s != "" {
				continue // keep the value already in the blob
			}
		}
		obj[k] = mustJSON(v)
	}
	return mustJSON(obj)
}

// toolEvents emits the KindToolCall + (when resolved) KindToolResult pair
// for one tool part, keyed on callID. pending/running tool parts emit only
// the call; completed/error emit both.
func toolEvents(m messageRow, p partRow, pd partData, partPayload json.RawMessage, slot event.Slot, ts time.Time, seq int64, version string, turnID *string, role string) []event.Event {
	callID := fallbackStr(pd.CallID, p.ID)

	var args json.RawMessage
	if pd.State != nil && len(pd.State.Input) > 0 {
		args = pd.State.Input
	}
	callContent := mustJSON(map[string]any{
		"tool": map[string]any{
			"id":   callID,
			"name": pd.Tool,
			"args": args,
		},
	})
	call := event.Event{
		EventUUID:     DeterministicEventUUID(m.SessionID, "call:"+callID),
		SessionID:     m.SessionID,
		Slot:          slot,
		Seq:           seq,
		Timestamp:     ts,
		Kind:          event.KindToolCall,
		SchemaVersion: 1,
		Content:       callContent,
		Vendor:        vendorFor(version, partPayload),
	}
	if role == "assistant" {
		call.TurnID = turnID
	}
	out := []event.Event{call}

	if pd.State == nil {
		return out
	}
	switch pd.State.Status {
	case "completed", "error":
		status := "success"
		var result any
		if pd.State.Status == "error" {
			status = "error"
			result = pd.State.Error
		} else {
			result = pd.State.Output
		}
		resultContent := mustJSON(map[string]any{
			"tool": map[string]any{
				"call_id": callID,
				"status":  status,
				"result":  result,
			},
		})
		res := event.Event{
			EventUUID:     DeterministicEventUUID(m.SessionID, "result:"+callID),
			SessionID:     m.SessionID,
			Slot:          slot,
			Seq:           seq + 1,
			Timestamp:     ts,
			Kind:          event.KindToolResult,
			SchemaVersion: 1,
			Content:       resultContent,
			Vendor:        vendorFor(version, partPayload),
		}
		out = append(out, res)
	}
	return out
}

// --- helpers ---

func vendorFor(version string, payload json.RawMessage) event.Vendor {
	v := event.Vendor{Name: Name, Version: version}
	if len(payload) > 0 {
		v.NativePayload = append(json.RawMessage(nil), payload...)
	}
	return v
}

// modelString renders a session/part model JSON blob ({"id","providerID",
// "variant"}) into "providerID/id", or "" when absent.
func modelString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var m SessionModel
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return joinModel(m.ProviderID, m.ID)
}

// messageModel pulls a model identity off a decoded message: assistant
// messages carry flat providerID/modelID; user messages carry a nested
// model object.
func messageModel(md messageData) string {
	if md.ProviderID != "" || md.ModelID != "" {
		return joinModel(md.ProviderID, md.ModelID)
	}
	return modelString(md.Model)
}

func joinModel(provider, id string) string {
	switch {
	case provider == "" && id == "":
		return ""
	case provider == "":
		return id
	case id == "":
		return provider
	}
	return provider + "/" + id
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Now().UTC()
	}
	return time.UnixMilli(ms).UTC()
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func fallbackStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// DeriveScope is the exported entry point for the scope-from-cwd resolver.
func DeriveScope(cwd string) string { return deriveScope(cwd) }

// deriveScope picks a scope path for an event given its cwd. Resolution
// order matches the claude adapter:
//  1. $RESLEEVE_SCOPE — verbatim override, if set.
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
