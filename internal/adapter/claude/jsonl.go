package claude

import "encoding/json"

// Record is one line from a Claude Code session JSONL file. Field
// names match the on-disk format (verified against
// ~/.claude/projects/*/<uuid>.jsonl, Claude Code 2.1.140).
type Record struct {
	Type           string          `json:"type"`
	UUID           string          `json:"uuid,omitempty"`
	ParentUUID     *string         `json:"parentUuid,omitempty"`
	SessionID      string          `json:"sessionId,omitempty"`
	Timestamp      string          `json:"timestamp,omitempty"`
	Cwd            string          `json:"cwd,omitempty"`
	GitBranch      string          `json:"gitBranch,omitempty"`
	Version        string          `json:"version,omitempty"`
	PermissionMode string          `json:"permissionMode,omitempty"`
	UserType       string          `json:"userType,omitempty"`
	Entrypoint     string          `json:"entrypoint,omitempty"`
	PromptID       string          `json:"promptId,omitempty"`
	IsSidechain    bool            `json:"isSidechain,omitempty"`
	Message        json.RawMessage `json:"message,omitempty"`
}

// ParseRecord decodes a JSONL line into a Record.
func ParseRecord(line []byte) (*Record, error) {
	var r Record
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// claudeContentBlock is one element of message.content for user /
// assistant records. Different fields are populated depending on Type.
type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result body (string or structured)
	IsError   bool            `json:"is_error,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

// userMessage is the inner shape of a user record's "message" field.
// Content is RawMessage because it can be either a string or an array
// of content blocks.
type userMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// assistantMessage is the inner shape of an assistant record's "message" field.
type assistantMessage struct {
	Model      string               `json:"model"`
	ID         string               `json:"id"`
	Role       string               `json:"role"`
	Content    []claudeContentBlock `json:"content"`
	StopReason string               `json:"stop_reason,omitempty"`
	Usage      json.RawMessage      `json:"usage,omitempty"`
}
