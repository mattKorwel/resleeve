package opencode

import "encoding/json"

// This file models the two opencode shapes resleeve touches:
//
//  1. The on-disk SQLite rows (SessionTable / MessageTable / PartTable),
//     read READ-ONLY by sqlite_read.go. Drizzle stores Message.data and
//     Part.data as JSON text with id/sessionID/messageID hoisted to their
//     own columns, so the row structs below re-attach those columns when
//     reconstructing the full SessionV1.Info / SessionV1.Part objects.
//
//  2. The `opencode import` transcript JSON
//     ({info, messages:[{info, parts}]}), emitted by tonative.go. The
//     import command (import.ts) decodes each message against
//     SessionV1.Info and each part against SessionV1.Part, so the import
//     structs mirror those discriminated unions closely enough to
//     round-trip a captured session.
//
// Verified against opencode dev @ 3151e22:
//   - session/sql.ts (SessionTable / MessageTable / PartTable columns)
//   - v1/session.ts (Info = User|Assistant, Part union, ToolState union)
//   - cli/cmd/import.ts (transform + insert; preserves info.id)

// --- import transcript JSON (tonative output) ---

// ImportFile is the top-level shape `opencode import <file>` decodes.
type ImportFile struct {
	Info     json.RawMessage `json:"info"` // SessionV1 session info
	Messages []ImportMessage `json:"messages"`
}

// ImportMessage is one {info, parts} entry. info is a SessionV1.Info
// (User or Assistant, discriminated on "role"); parts is a SessionV1.Part
// array (discriminated on "type").
type ImportMessage struct {
	Info  json.RawMessage   `json:"info"`
	Parts []json.RawMessage `json:"parts"`
}

// SessionInfo is the subset of SessionV1 session info resleeve constructs
// when no native payload is available. opencode validates additional
// fields but import.ts overrides directory/path/projectID itself, so a
// minimal-but-valid info is acceptable for replay. Fields we always know
// from capture are populated; the rest are emitted via raw payload when
// present (preferred) so the shape matches byte-for-byte.
type SessionInfo struct {
	ID        string          `json:"id"`
	Slug      string          `json:"slug"`
	ProjectID string          `json:"projectID,omitempty"`
	Directory string          `json:"directory"`
	Title     string          `json:"title"`
	Version   string          `json:"version"`
	Model     *SessionModel   `json:"model,omitempty"`
	Time      SessionInfoTime `json:"time"`
}

// SessionModel mirrors SessionV1 SessionModel ({id, providerID, variant?}).
type SessionModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

// SessionInfoTime is the session info time struct ({created, updated}).
type SessionInfoTime struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
}

// --- SQLite row structs (sqlite_read input) ---

// sessionRow is a SessionTable row, selected columns only.
type sessionRow struct {
	ID          string
	ProjectID   string
	Directory   string
	Title       string
	Version     string
	Model       json.RawMessage // {"id","providerID","variant"} or null
	TimeCreated int64
	TimeUpdated int64
}

// messageRow is a MessageTable row: id + session_id columns plus the
// JSON data blob (a SessionV1.Info minus id/sessionID).
type messageRow struct {
	ID          string
	SessionID   string
	TimeCreated int64
	Data        json.RawMessage
}

// partRow is a PartTable row: id + message_id + session_id columns plus
// the JSON data blob (a SessionV1.Part minus id/sessionID/messageID).
type partRow struct {
	ID          string
	MessageID   string
	SessionID   string
	TimeCreated int64
	Data        json.RawMessage
}

// --- decoded part / message bodies (sqlite_read + sse) ---

// messageData is the decoded Message.data blob. role discriminates
// user/assistant. model/modelID carry the model identity (user messages
// carry a nested model object; assistant messages carry flat modelID/
// providerID).
type messageData struct {
	Role       string          `json:"role"`
	ModelID    string          `json:"modelID,omitempty"`
	ProviderID string          `json:"providerID,omitempty"`
	Model      json.RawMessage `json:"model,omitempty"`
	Time       messageDataTime `json:"time"`
}

type messageDataTime struct {
	Created int64 `json:"created"`
}

// partData is the decoded Part.data blob. type discriminates the part
// kind. Only the fields resleeve maps are decoded; the rest survive in
// the raw payload.
type partData struct {
	Type   string     `json:"type"`
	Text   string     `json:"text,omitempty"`   // text / reasoning parts
	CallID string     `json:"callID,omitempty"` // tool parts
	Tool   string     `json:"tool,omitempty"`   // tool parts
	State  *toolState `json:"state,omitempty"`  // tool parts
}

// toolState is the discriminated tool-call state. status ∈ {pending,
// running, completed, error}. completed carries input+output; error
// carries input+error.
type toolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}
