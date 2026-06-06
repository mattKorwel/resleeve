package antigravity

import (
	"encoding/json"
	"strings"

	"github.com/mattkorwel/resleeve/internal/event"
)

// step is one decoded row of the conversation .db `steps` table. Only the
// columns the mapper needs are carried; the raw protobuf payload is retained
// for vendor.NativePayload and for re-decoding.
type step struct {
	Idx      int64
	StepType int64
	Status   int64
	Payload  []byte
}

// step_type classification.
//
// These integer codes were reverse-engineered by opening the three real
// conversation databases under ~/.gemini/antigravity-cli/conversations and
// correlating each step's decoded protobuf strings with its role:
//
//	14  user message      (user prompt text at proto path [19,2]/[19,3,1])
//	15  assistant message (model text at [20,1]/[20,8]; planning at [20,3])
//	7,8,9,21,33,132  tool call (tool name + JSON args with toolAction/Summary)
//	98  context injection  (a "# Conversation History" system block)
//	23  trajectory title   (a short generated session title)
//
// Anything else is emitted as KindSystem carrying the raw payload rather
// than guessed at — see classify. The set is deliberately conservative; new
// undocumented step_types will surface as system events, never as
// mis-attributed user/assistant text.
const (
	stUser      = 14
	stAssistant = 15
	stContext   = 98
	stTitle     = 23
)

// toolStepTypes are the step_type codes observed to carry tool calls. Every
// one decoded with a tool name at proto path [5,4,2] and a JSON args object
// (keys toolAction / toolSummary) at [5,4,3].
var toolStepTypes = map[int64]bool{
	7: true, 8: true, 9: true, 21: true, 33: true, 132: true,
}

// classify maps a step_type to a normalized event.Kind. The second result is
// false when we are NOT confident — the caller then emits KindSystem so the
// raw payload is preserved without claiming a role.
func classify(stepType int64) (event.Kind, bool) {
	switch stepType {
	case stUser:
		return event.KindUserMessage, true
	case stAssistant:
		return event.KindAssistantMessage, true
	case stContext, stTitle:
		return event.KindSystem, true
	}
	if toolStepTypes[stepType] {
		return event.KindToolCall, true
	}
	return event.KindSystem, false
}

// userTextPaths are the proto paths where the verbatim user prompt appears
// in a step_type 14 payload. The same text is duplicated; we take the first
// match. Path [19,2] is the prompt; [19,3,1] is an echo.
var userTextPaths = [][]int{{19, 2}, {19, 3, 1}}

// assistantTextPaths are the proto paths carrying assistant response text in
// a step_type 15 payload. [20,1] / [20,8] are the rendered answer; [20,3] is
// the model's planning/thinking preamble (used only as a fallback).
var assistantTextPaths = [][]int{{20, 1}, {20, 8}, {20, 3}}

// extractText returns the best human-readable text for a step given the
// candidate proto paths, falling back to the single longest string leaf when
// no candidate path matches (schema drift insurance).
func extractText(leaves []stringLeaf, candidates [][]int) string {
	for _, want := range candidates {
		for _, l := range leaves {
			if pathEqual(l.Path, want) {
				if s := strings.TrimSpace(l.Value); s != "" {
					return s
				}
			}
		}
	}
	// Fallback: longest leaf that isn't an obvious UUID/path/id.
	best := ""
	for _, l := range leaves {
		v := strings.TrimSpace(l.Value)
		if looksLikeNoise(v) {
			continue
		}
		if len(v) > len(best) {
			best = v
		}
	}
	return best
}

// toolNamePaths / toolArgsPaths locate a tool call's name and JSON args.
var toolNamePaths = [][]int{{5, 4, 2}, {5, 4, 9}}
var toolArgsPaths = [][]int{{5, 4, 3}}

// extractTool pulls the tool name and (best-effort) JSON args from a tool
// step's decoded leaves.
func extractTool(leaves []stringLeaf) (name string, args json.RawMessage) {
	name = firstAtPaths(leaves, toolNamePaths)
	if raw := firstAtPaths(leaves, toolArgsPaths); raw != "" && json.Valid([]byte(raw)) {
		args = json.RawMessage(raw)
	}
	return name, args
}

// firstAtPaths returns the first non-empty trimmed leaf whose path matches
// any of the candidate paths, in candidate order.
func firstAtPaths(leaves []stringLeaf, candidates [][]int) string {
	for _, want := range candidates {
		for _, l := range leaves {
			if pathEqual(l.Path, want) {
				if s := strings.TrimSpace(l.Value); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func pathEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// looksLikeNoise reports whether a string is an id / path / token rather
// than conversational text. Used only by the longest-leaf fallback.
func looksLikeNoise(s string) bool {
	if s == "" {
		return true
	}
	if isUUIDish(s) {
		return true
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "file:///") {
		return true
	}
	if !strings.ContainsAny(s, " \t\n") && len(s) < 24 {
		return true // single short token, e.g. an opaque id
	}
	return false
}

// isUUIDish reports whether s is shaped like a UUID (8-4-4-4-12 hex).
func isUUIDish(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexDigit(c) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
