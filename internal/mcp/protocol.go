// Package mcp implements a minimal Model Context Protocol server for
// resleeve. It speaks JSON-RPC 2.0 over stdio (per the MCP spec,
// https://modelcontextprotocol.io/specification), wired to the local
// resleeve daemon for memory CRUD.
//
// Supported methods:
//
//	initialize                 capability handshake; returns instructions
//	notifications/initialized  no-op (client ack)
//	tools/list                 returns the tool catalog
//	tools/call                 invokes a tool, returns content
//	ping                       liveness probe
//
// Tools wrap the daemon's HTTP API (see internal/agent/memory_client.go).
// Adding a tool is one entry in registerTools() plus a handler.
//
// The `instructions` field in the initialize response is the documented
// mechanism MCP hosts (Claude Code, opencode, codex) consume to inject
// standing system-prompt content; opencode renders it as a visible
// first system message, which is why MCP complements (not replaces)
// the SessionStart hook.
package mcp

import "encoding/json"

const (
	jsonRPCVersion = "2.0"
	mcpProtocolVer = "2024-11-05"
	implName       = "resleeve"
	implVersion    = "0.1.0"
)

// rpcRequest is a JSON-RPC 2.0 request frame.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // null/missing for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response frame. Either Result or Error
// must be set; never both.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC standard error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

// initializeResult is what we return on the MCP initialize handshake.
//
// Instructions is a free-form markdown string the host (Claude Code,
// opencode, codex) injects into the agent's system prompt verbatim.
// Added in MCP protocol 2024-11-05. resleeve uses it to ship the
// standing curation rules + the rolled-up plan/learnings for the
// session's resolved scope (see server.buildInstructions).
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    capabilities   `json:"capabilities"`
	ServerInfo      implementation `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

type capabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// toolDescriptor is what tools/list returns per tool.
type toolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// toolsListResult is the tools/list response payload.
type toolsListResult struct {
	Tools []toolDescriptor `json:"tools"`
}

// toolCallParams is the tools/call request payload.
type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolCallResult is the tools/call response payload. Content is
// MCP-shaped: an array of {type, text} blocks.
type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// contentBlock is one MCP content item.
//
// IMPORTANT: Text is `json:"text"` WITHOUT `omitempty`. The MCP spec
// requires the `text` field to be present whenever `type=="text"`,
// even when the value is the empty string. Zod-generated client
// validators (Claude Code et al.) reject the block when `text` is
// absent — they walk the union {text|image|audio|resource} looking
// for a match and fail on every alternate, surfacing as a confusing
// "expected string, received undefined" error to the operator. We
// also avoid emitting truly empty strings from handlers (return a
// friendly placeholder instead). Same lesson ori learned in 2026-05.
type contentBlock struct {
	Type string `json:"type"` // "text" for now
	Text string `json:"text"`
}

// textBlock is a tiny constructor; most tools return a single text block.
func textBlock(s string) []contentBlock {
	return []contentBlock{{Type: "text", Text: s}}
}
