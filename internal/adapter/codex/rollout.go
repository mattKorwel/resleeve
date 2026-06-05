package codex

import "encoding/json"

// RolloutLine is one line of a Codex rollout JSONL file. Each line is
// {"timestamp", "type", "payload"} — `type` discriminates the payload
// shape (verified protocol/src/protocol.rs RolloutLine + RolloutItem,
// snake_case tag/content).
//
//	type ∈ {session_meta, response_item, compacted, turn_context, event_msg}
type RolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// ParseRolloutLine decodes a rollout JSONL line into a RolloutLine.
func ParseRolloutLine(line []byte) (*RolloutLine, error) {
	var r RolloutLine
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SessionMetaPayload is the payload of a `session_meta` line. It flattens
// Codex's SessionMeta with the sibling `git` block (SessionMetaLine).
// The session id (UUIDv7) and cwd live here; the git branch lives in
// git.branch. Note: there is NO model on this line — model is read from
// turn_context.
type SessionMetaPayload struct {
	ID         string   `json:"id"`
	Timestamp  string   `json:"timestamp,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	Originator string   `json:"originator,omitempty"`
	CLIVersion string   `json:"cli_version,omitempty"`
	Source     string   `json:"source,omitempty"`
	Git        *GitInfo `json:"git,omitempty"`
}

// GitInfo is the flattened git block on a session_meta payload.
type GitInfo struct {
	CommitHash    string `json:"commit_hash,omitempty"`
	Branch        string `json:"branch,omitempty"`
	RepositoryURL string `json:"repository_url,omitempty"`
}

// TurnContextPayload is the payload of a `turn_context` line. The model
// is read from here (never session_meta); the first turn_context sets the
// session model.
type TurnContextPayload struct {
	TurnID         string `json:"turn_id,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	Model          string `json:"model,omitempty"`
	ApprovalPolicy string `json:"approval_policy,omitempty"`
}

// ResponseItemPayload is a partially-decoded OpenAI Responses-API item.
// Only the fields resleeve needs to classify and normalize are pulled
// out; the full raw line is preserved in vendor.native_payload for
// byte-faithful replay.
//
//	Message            → {role, content[]}
//	FunctionCall       → {name, arguments(string), call_id}
//	FunctionCallOutput → {call_id, output}
//	CustomToolCall*    → similar tool-call/output shapes
type ResponseItemPayload struct {
	Type      string                `json:"type"`
	ID        string                `json:"id,omitempty"`
	Role      string                `json:"role,omitempty"`
	Content   []ResponseContentItem `json:"content,omitempty"`
	Name      string                `json:"name,omitempty"`
	Arguments json.RawMessage       `json:"arguments,omitempty"`
	Input     json.RawMessage       `json:"input,omitempty"`
	CallID    string                `json:"call_id,omitempty"`
	Output    json.RawMessage       `json:"output,omitempty"`
}

// ResponseContentItem is one element of a Message item's content array.
// Variants: input_text / output_text carry `text`; input_image carries
// `image_url`.
type ResponseContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// EventMsgPayload is the payload of an `event_msg` UI event line. The tag
// `type` selects the variant; resleeve only materializes `user_message`
// (and drops the rest as non-content UI noise on the JSONL path).
type EventMsgPayload struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
	TurnID  string `json:"turn_id,omitempty"`
}

// CompactedPayload is the payload of a `compacted` line — a compaction
// marker. Recorded but not expanded.
type CompactedPayload struct {
	Message string `json:"message,omitempty"`
}
