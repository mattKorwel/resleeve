package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/agent"
	"github.com/mattkorwel/resleeve/internal/agent/agenttest"
	"github.com/mattkorwel/resleeve/internal/mcp"
	"github.com/mattkorwel/resleeve/internal/memory"
)

// memScopePtr is a tiny builder for *memory.Scope; the PutScope
// client wants a pointer, this keeps test call sites terse.
func memScopePtr(path, kind, title string) *memory.Scope {
	return &memory.Scope{Path: path, Kind: memory.ScopeKind(kind), Title: title}
}

// newDaemonClient spins up the real agent HTTP mux backed by an
// in-memory sqlite store (via agenttest.TestHandler), fronts it with
// an httptest.Server, and returns a configured *agent.Client. This
// exercises the real daemon stack — same routes, same auth — without
// the production Serve()'s side effects (listener, endpoint file,
// reconcile goroutines, stdout banners).
func newDaemonClient(t *testing.T) *agent.Client {
	t.Helper()
	const secret = "test-secret-mcp"
	mux, cleanup, err := agenttest.TestHandler(context.Background(), "", secret)
	if err != nil {
		t.Fatalf("agenttest.TestHandler: %v", err)
	}
	hs := httptest.NewServer(mux)
	t.Cleanup(func() {
		hs.Close()
		cleanup()
	})
	return agent.NewClient(hs.URL, secret)
}

// driveStdio runs the MCP server with the given input lines and
// returns the response lines. The server reads the canned input
// stream, dispatches each frame, writes JSON-RPC responses to a
// buffer, then exits on EOF. Bounded by a 5-second ctx so a hung
// server can't wedge the suite.
func driveStdio(t *testing.T, cfg mcp.Config, inputLines ...string) []string {
	t.Helper()
	srv := mcp.New(cfg)
	in := strings.NewReader(strings.Join(inputLines, "\n") + "\n")
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.ServeStdio(ctx, in, &out); err != nil {
		// EOF returns nil; other errors are real.
		t.Fatalf("ServeStdio: %v", err)
	}
	raw := strings.TrimRight(out.String(), "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// TestStdioInitializeListCall exercises the canonical session
// handshake — initialize → tools/list → tools/call — over the stdio
// transport. This is the end-to-end loop the brief asks for: it
// validates frame parsing, JSON-RPC plumbing, tool registration, and
// the agent.Client integration in one pass.
func TestStdioInitializeListCall(t *testing.T) {
	c := newDaemonClient(t)
	cfg := mcp.Config{Client: c}

	resp := driveStdio(t, cfg,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"resleeve_scope_set","arguments":{"path":"resleeve","kind":"project","title":"Resleeve"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"resleeve_plan_write","arguments":{"scope":"resleeve","content":"the plan"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"resleeve_plan_read","arguments":{"scope":"resleeve"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"resleeve_learning_append","arguments":{"scope":"resleeve","content":"learned X"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"resleeve_learning_list","arguments":{"scope":"resleeve"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"resleeve_scope_list","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"resleeve_context","arguments":{"scope":"resleeve"}}}`,
	)

	// notifications/initialized has no response — so we expect 9
	// frames back (one per request with an id), not 10.
	if len(resp) != 9 {
		t.Fatalf("expected 9 responses, got %d:\n%s", len(resp), strings.Join(resp, "\n"))
	}

	// 1. initialize: must return protocolVersion + non-empty instructions.
	init := decodeResp(t, resp[0])
	result, _ := init["result"].(map[string]any)
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion: got %v want 2024-11-05", result["protocolVersion"])
	}
	instructions, _ := result["instructions"].(string)
	if !strings.Contains(instructions, "resleeve") {
		t.Fatalf("instructions missing standing prompt; got: %q", instructions)
	}

	// 2. tools/list: must include all 10 declared tools.
	list := decodeResp(t, resp[1])
	listResult, _ := list["result"].(map[string]any)
	tools, _ := listResult["tools"].([]any)
	if len(tools) != 10 {
		t.Fatalf("expected 10 tools; got %d", len(tools))
	}
	names := map[string]bool{}
	for _, x := range tools {
		tm := x.(map[string]any)
		names[tm["name"].(string)] = true
	}
	for _, want := range []string{
		"resleeve_scope_set", "resleeve_scope_get", "resleeve_scope_list", "resleeve_scope_delete",
		"resleeve_plan_write", "resleeve_plan_read", "resleeve_plan_list",
		"resleeve_learning_append", "resleeve_learning_list",
		"resleeve_context",
	} {
		if !names[want] {
			t.Errorf("missing tool: %s", want)
		}
	}

	// 3. scope_set ack
	requireOK(t, resp[2], "scope")
	// 4. plan_write ack
	requireOK(t, resp[3], "plan")
	// 5. plan_read should echo "the plan"
	requireContains(t, resp[4], "the plan")
	// 6. learning_append ack
	requireOK(t, resp[5], "learning")
	// 7. learning_list should contain our learning
	requireContains(t, resp[6], "learned X")
	// 8. scope_list should have the resleeve scope
	requireContains(t, resp[7], "resleeve")
	// 9. context should include both the plan and the learning
	ctxResp := decodeResp(t, resp[8])
	ctxResult, _ := ctxResp["result"].(map[string]any)
	if isErr, _ := ctxResult["isError"].(bool); isErr {
		t.Errorf("resleeve_context returned isError: %v", ctxResult)
	}
}

// TestAutoLoadedScopeContext verifies that when DefaultScope is set,
// the initialize handshake's instructions blob includes both the
// standing prompt AND a "## Auto-loaded scope context:" section
// populated from the daemon's rolled-up context.
func TestAutoLoadedScopeContext(t *testing.T) {
	c := newDaemonClient(t)

	// Seed the daemon with a scope + plan so GetContext has something
	// non-empty to return.
	ctx := context.Background()
	if _, err := c.PutScope(ctx, memScopePtr("resleeve", "project", "Resleeve")); err != nil {
		t.Fatalf("seed PutScope: %v", err)
	}
	if _, err := c.PutPlan(ctx, "resleeve", "_default", "## Now\nbuild the mcp server"); err != nil {
		t.Fatalf("seed PutPlan: %v", err)
	}

	cfg := mcp.Config{Client: c, DefaultScope: "resleeve"}
	resp := driveStdio(t, cfg,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
	)
	if len(resp) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resp))
	}
	init := decodeResp(t, resp[0])
	result, _ := init["result"].(map[string]any)
	instructions, _ := result["instructions"].(string)
	if !strings.Contains(instructions, "Auto-loaded scope context") {
		t.Fatalf("instructions missing auto-loaded section:\n%s", instructions)
	}
	if !strings.Contains(instructions, "build the mcp server") {
		t.Fatalf("instructions missing seeded plan content:\n%s", instructions)
	}
	if !strings.Contains(instructions, "resleeve") {
		t.Fatalf("instructions missing scope name")
	}
}

// TestUnknownToolReturnsError verifies a missing tool surfaces as a
// JSON-RPC error (not a silent empty result).
func TestUnknownToolReturnsError(t *testing.T) {
	c := newDaemonClient(t)
	resp := driveStdio(t, mcp.Config{Client: c},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)
	if len(resp) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(resp))
	}
	frame := decodeResp(t, resp[1])
	if _, hasErr := frame["error"]; !hasErr {
		t.Fatalf("expected JSON-RPC error for unknown tool, got: %v", frame)
	}
}

// TestPlanReadEmptyHasTextField regresses the Zod-validator-friendly
// content-block shape: every text block must include a non-omitempty
// `text` field, AND the value must be non-empty (we emit a friendly
// placeholder instead of ""). Same lesson ori shipped in 2026-05.
func TestPlanReadEmptyHasTextField(t *testing.T) {
	c := newDaemonClient(t)
	ctx := context.Background()
	if _, err := c.PutScope(ctx, memScopePtr("solo", "project", "Solo")); err != nil {
		t.Fatalf("seed PutScope: %v", err)
	}

	resp := driveStdio(t, mcp.Config{Client: c},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"resleeve_plan_read","arguments":{"scope":"solo","inherit":true}}}`,
	)
	if len(resp) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(resp))
	}
	frame := decodeResp(t, resp[1])
	result, _ := frame["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block; got %d", len(content))
	}
	first := content[0].(map[string]any)
	textVal, present := first["text"]
	if !present {
		t.Fatalf("text field missing — omitempty regression")
	}
	textStr, ok := textVal.(string)
	if !ok {
		t.Fatalf("text field not a string: %T", textVal)
	}
	if textStr == "" {
		t.Fatalf("text field empty — friendly-placeholder regression")
	}
}

// --- tiny test helpers ---

func decodeResp(t *testing.T, line string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if got["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc mismatch: %v", got["jsonrpc"])
	}
	return got
}

func requireOK(t *testing.T, line, wantSubstr string) {
	t.Helper()
	frame := decodeResp(t, line)
	if errObj, hasErr := frame["error"]; hasErr {
		t.Fatalf("expected ok, got error: %v", errObj)
	}
	result, _ := frame["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tool returned isError=true: %v", result)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content blocks: %v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.Contains(text, wantSubstr) {
		t.Errorf("ok message missing %q: %q", wantSubstr, text)
	}
}

func requireContains(t *testing.T, line, wantSubstr string) {
	t.Helper()
	frame := decodeResp(t, line)
	if errObj, hasErr := frame["error"]; hasErr {
		t.Fatalf("expected ok, got error: %v", errObj)
	}
	result, _ := frame["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content blocks: %v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.Contains(text, wantSubstr) {
		t.Errorf("content missing %q: %q", wantSubstr, text)
	}
}
