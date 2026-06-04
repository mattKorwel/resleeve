package mcp

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/mattkorwel/resleeve/internal/agent"
)

// instructionsMarkdown is the standing system-prompt guidance the MCP
// server hands the host on `initialize`. Edit INSTRUCTIONS.md in this
// package to change it; the embed is rebuilt at compile.
//
//go:embed INSTRUCTIONS.md
var instructionsMarkdown string

// instructionsCap is the per-section max bytes (standing prompt +
// auto-loaded scope context) we inline into the initialize handshake.
// Big enough for typical resleeve plans + a few learnings; small
// enough that an 80 KB log doesn't blow the host's instructions
// buffer. Matches the ori 8 KB convention.
const instructionsCap = 8 * 1024

// Config wires the server to the local daemon.
type Config struct {
	// Client is the agent.Client pointed at the local daemon. Required.
	Client *agent.Client

	// DefaultScope is the scope resolved at boot from RESLEEVE_SCOPE /
	// .resleeve-scope marker / filepath.Base(cwd). Used to:
	//   - fill in the "Auto-loaded scope context" section of the
	//     initialize instructions blob, and
	//   - default missing `scope` args on tool calls.
	// Empty string is OK — server runs but doesn't auto-load.
	DefaultScope string

	// Logger receives stderr-style log lines. The MCP stdio transport
	// reserves stdout for JSON-RPC; NEVER log to stdout. nil is OK
	// (logs are dropped).
	Logger func(format string, args ...any)

	// ServerName / ServerVersion override defaults in the initialize
	// serverInfo block.
	ServerName    string
	ServerVersion string
}

// Server runs the MCP loop.
type Server struct {
	cfg   Config
	tools map[string]*tool
}

// New constructs a Server with the resleeve tool surface registered.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = func(string, ...any) {}
	}
	if cfg.ServerName == "" {
		cfg.ServerName = implName
	}
	if cfg.ServerVersion == "" {
		cfg.ServerVersion = implVersion
	}
	s := &Server{cfg: cfg, tools: map[string]*tool{}}
	s.registerTools()
	return s
}

// ServeStdio reads JSON-RPC frames from r (newline-delimited), writes
// responses to w, and exits cleanly on EOF or ctx cancel.
//
// stdout safety: callers MUST give this method the real os.Stdout for
// production use; anything else logged to stdout will corrupt the
// JSON-RPC frame stream. The CLI verb sets c.Logger to write to
// stderr / a file so library code stays neutral.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	// Lock writes so concurrent goroutines (notifications, etc.) don't
	// interleave bytes on stdout.
	var wmu sync.Mutex
	encode := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		buf, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s\n", buf)
		return err
	}

	br := bufio.NewReader(r)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := br.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		// Skip blank keepalive lines.
		if len(line) <= 1 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.cfg.Logger("decode error: %v", err)
			_ = encode(rpcResponse{
				JSONRPC: jsonRPCVersion,
				Error: &rpcError{
					Code:    rpcParseError,
					Message: err.Error(),
				},
			})
			continue
		}

		resp := s.dispatch(ctx, &req)
		if resp == nil {
			// Notification (no id) — nothing to send back.
			continue
		}
		if err := encode(resp); err != nil {
			s.cfg.Logger("encode error: %v", err)
		}
	}
}

// dispatch routes one request. Returns nil for notifications.
func (s *Server) dispatch(ctx context.Context, req *rpcRequest) *rpcResponse {
	if req.JSONRPC != jsonRPCVersion {
		return errResp(req.ID, rpcInvalidRequest, "unsupported jsonrpc version")
	}
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		return okResp(req.ID, initializeResult{
			ProtocolVersion: mcpProtocolVer,
			Capabilities: capabilities{
				Tools: &toolsCapability{},
			},
			ServerInfo: implementation{
				Name:    s.cfg.ServerName,
				Version: s.cfg.ServerVersion,
			},
			Instructions: s.buildInstructions(ctx),
		})

	case "notifications/initialized":
		// Per spec; client ack.
		return nil

	case "ping":
		return okResp(req.ID, map[string]any{})

	case "tools/list":
		descs := make([]toolDescriptor, 0, len(s.tools))
		for _, t := range s.tools {
			descs = append(descs, toolDescriptor{
				Name:        t.name,
				Description: t.description,
				InputSchema: t.inputSchema,
			})
		}
		return okResp(req.ID, toolsListResult{Tools: descs})

	case "tools/call":
		var p toolCallParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return errResp(req.ID, rpcInvalidParams, err.Error())
			}
		}
		t, ok := s.tools[p.Name]
		if !ok {
			return errResp(req.ID, rpcMethodNotFound, "unknown tool: "+p.Name)
		}
		out, err := t.handler(ctx, s.cfg.Client, s.cfg.DefaultScope, p.Arguments)
		if err != nil {
			// Per MCP, tool errors come back as a result with isError=true,
			// not as a JSON-RPC error — so the model can see and recover.
			return okResp(req.ID, toolCallResult{
				Content: textBlock(err.Error()),
				IsError: true,
			})
		}
		return okResp(req.ID, out)

	default:
		if isNotification {
			return nil
		}
		return errResp(req.ID, rpcMethodNotFound, "unknown method: "+req.Method)
	}
}

// buildInstructions concatenates the standing INSTRUCTIONS.md prompt
// with an auto-loaded "## Auto-loaded scope context: <scope>" section
// containing the daemon-rendered rolled-up plan+learnings for the
// resolved default scope (with parent inheritance).
//
// The instructions field is the MCP-documented mechanism hosts use to
// inject standing system-prompt content. opencode renders it visibly,
// Claude Code/codex silently — but in all three it lands in the
// model's context at session start. This is the parity-with-ori
// behavior the user wanted (silent F13 hook injection + visible MCP
// instructions are complementary, not redundant).
//
// Each section is capped at instructionsCap (8 KB) so a runaway plan
// or learnings log can't dwarf the host's context budget. Failures
// (daemon down, scope missing) degrade silently to just the standing
// prompt — the standing prompt itself documents the tools so the
// agent can fetch on demand.
func (s *Server) buildInstructions(ctx context.Context) string {
	standing := truncateToCap(instructionsMarkdown, instructionsCap, "\n\n_[truncated]_\n")
	scope := strings.TrimSpace(s.cfg.DefaultScope)
	if scope == "" {
		return standing
	}

	body, err := s.cfg.Client.GetContext(ctx, scope)
	if err != nil {
		s.cfg.Logger("auto-load context for %s failed: %v", scope, err)
		return standing
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return standing
	}

	var b strings.Builder
	b.WriteString(standing)
	if !strings.HasSuffix(standing, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("\n---\n\n")
	b.WriteString("## Auto-loaded scope context: `")
	b.WriteString(scope)
	b.WriteString("`\n\n")
	b.WriteString("Plan + recent learnings for the scope this session was launched at, with inheritance from parent scopes. Use it as your starting frame; refresh later via `resleeve_plan_read` / `resleeve_learning_list` with `inherit=true`.\n\n")
	b.WriteString(truncateToCap(body, instructionsCap, "\n\n_[truncated; fetch via `resleeve_context` for full content]_\n"))
	return b.String()
}

func okResp(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Result:  result,
	}
}

func errResp(id json.RawMessage, code int, msg string) *rpcResponse {
	return &rpcResponse{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: msg,
		},
	}
}

// StderrLogger is a convenience for the CLI verb: stamps lines on
// os.Stderr with a "resleeve-mcp: " prefix. Use this when nothing
// fancier is wired (the daemon log lives in ~/.resleeve/daemon.log
// and isn't this server's concern).
func StderrLogger(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "resleeve-mcp: "+format+"\n", args...)
}
