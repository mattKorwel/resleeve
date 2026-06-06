package antigravity

import (
	"encoding/json"

	"github.com/mattkorwel/resleeve/internal/event"
)

// mapStep converts one decoded step row into zero or more normalized events.
// It is the heart of the antigravity capture path. Every step yields exactly
// one event; the event's Kind comes from classify(step_type) and its Content
// from the protobuf-decoded string leaves.
//
// Determinism: the event UUID is keyed on (conversationID, "step:"+idx), so
// re-running reconcile over the same db produces identical events.
//
// Robustness: a panic while decoding a single step is recovered here so the
// surrounding batch survives — one bad payload never loses a whole
// conversation. The raw payload is ALWAYS preserved in vendor.NativePayload.
func (a *Adapter) mapStep(convID, scope string, s step) (out []event.Event) {
	defer func() {
		if r := recover(); r != nil {
			// Degrade to a system event carrying the raw payload.
			out = []event.Event{a.systemEvent(convID, scope, s, "panic")}
		}
	}()

	leaves := DecodeStrings(s.Payload)
	kind, confident := classify(s.StepType)

	base := event.Event{
		EventUUID:     DeterministicEventUUID(convID, stepKey(s.Idx)),
		SessionID:     convID,
		Slot:          event.Slot{Scope: scope, AgentName: "default"},
		Seq:           s.Idx, // idx is the conversation's native total order
		Kind:          kind,
		SchemaVersion: 1,
		Vendor: event.Vendor{
			Name:          Name,
			NativePayload: append(json.RawMessage(nil), s.Payload...),
		},
	}

	if !confident {
		// Unknown step_type: keep the payload, claim nothing about role.
		return []event.Event{a.systemEvent(convID, scope, s, "unclassified")}
	}

	switch kind {
	case event.KindUserMessage:
		base.Content = mustJSON(map[string]any{
			"text":          extractText(leaves, userTextPaths),
			"gen_ai.system": "antigravity",
			"step_type":     s.StepType,
		})
	case event.KindAssistantMessage:
		base.Content = mustJSON(map[string]any{
			"text":          extractText(leaves, assistantTextPaths),
			"gen_ai.system": "antigravity",
			"step_type":     s.StepType,
		})
	case event.KindToolCall:
		name, args := extractTool(leaves)
		base.Content = mustJSON(map[string]any{
			"tool": map[string]any{
				"name": name,
				"args": args,
			},
			"step_type": s.StepType,
		})
	case event.KindSystem:
		// Confidently-classified system blocks (context injection / title):
		// keep the longest recovered text for human readability.
		base.Content = mustJSON(map[string]any{
			"resleeve.system.kind": systemKindName(s.StepType),
			"text":                 extractText(leaves, nil),
			"step_type":            s.StepType,
		})
	default:
		base.Content = mustJSON(map[string]any{"step_type": s.StepType})
	}
	return []event.Event{base}
}

// systemEvent builds a KindSystem event that preserves the raw payload for a
// step we could not (or chose not to) interpret. reason distinguishes
// "unclassified" (unknown step_type) from "panic" (decoder blew up).
func (a *Adapter) systemEvent(convID, scope string, s step, reason string) event.Event {
	content := mustJSON(map[string]any{
		"resleeve.system.kind": "antigravity:" + reason,
		"step_type":            s.StepType,
	})
	return event.Event{
		EventUUID:     DeterministicEventUUID(convID, stepKey(s.Idx)),
		SessionID:     convID,
		Slot:          event.Slot{Scope: scope, AgentName: "default"},
		Seq:           s.Idx,
		Kind:          event.KindSystem,
		SchemaVersion: 1,
		Content:       content,
		Vendor: event.Vendor{
			Name:          Name,
			NativePayload: append(json.RawMessage(nil), s.Payload...),
		},
	}
}

// systemKindName labels a confidently-classified system step for content.
func systemKindName(stepType int64) string {
	switch stepType {
	case stContext:
		return "antigravity:context"
	case stTitle:
		return "antigravity:title"
	default:
		return "antigravity:system"
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
