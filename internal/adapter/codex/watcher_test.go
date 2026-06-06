package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// captureIngester records all ingested events grouped by session id.
type captureIngester struct {
	bySession map[string][]event.Event
}

func newCaptureIngester() *captureIngester {
	return &captureIngester{bySession: map[string][]event.Event{}}
}

func (c *captureIngester) IngestBatch(ctx context.Context, sessionID string, events []event.Event) error {
	c.bySession[sessionID] = append(c.bySession[sessionID], events...)
	return nil
}

// sampleRollout mirrors a real Codex rollout: a user turn is written TWICE
// — once as a response_item message(role=user) (the superset the model
// sees, recorded as KindSystem) and once as an event_msg user_message (the
// canonical clean prompt, recorded as the single KindUserMessage). Both
// carry the same text "do the thing"; the adapter must NOT double-ingest
// it as two user messages.
const sampleRollout = `{"timestamp":"2026-06-04T03:17:25.909Z","type":"session_meta","payload":{"id":"019e90a2-c115-7523-b5a3-50c024b3ba14","timestamp":"2026-06-04T03:17:25.909Z","cwd":"/home/u/proj","cli_version":"0.137.0","source":"cli","git":{"branch":"dev"}}}
{"timestamp":"2026-06-04T03:17:26.0Z","type":"turn_context","payload":{"turn_id":"T1","cwd":"/home/u/proj","model":"gpt-5-codex","approval_policy":"on-request"}}
{"timestamp":"2026-06-04T03:17:27.0Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do the thing"}]}}
{"timestamp":"2026-06-04T03:17:27.5Z","type":"event_msg","payload":{"type":"user_message","message":"do the thing"}}
{"timestamp":"2026-06-04T03:17:28.0Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls\"}","call_id":"call_1"}}
{"timestamp":"2026-06-04T03:17:29.0Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"file.txt"}}
{"timestamp":"2026-06-04T03:17:30.0Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"listed files"}]}}
{"timestamp":"2026-06-04T03:17:31.0Z","type":"event_msg","payload":{"type":"token_count","info":null}}
`

func writeSampleRollout(t *testing.T, codexHomeDir string) string {
	t.Helper()
	dir := filepath.Join(codexHomeDir, "sessions", "2026", "06", "04")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	path := filepath.Join(dir, "rollout-2026-06-04T03-17-25-019e90a2-c115-7523-b5a3-50c024b3ba14.jsonl")
	if err := os.WriteFile(path, []byte(sampleRollout), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

// coTimestampRollout has THREE response_item lines sharing one millisecond
// timestamp (…:27.100Z). Reconcile must keep them in file order on replay;
// the regression is that Seq=ts.UnixNano() collides and the (Seq,EventUUID)
// sort reorders them by deterministic UUID.
const coTimestampRollout = `{"timestamp":"2026-06-04T03:17:25.0Z","type":"session_meta","payload":{"id":"019e90a2-c115-7523-b5a3-50c024b3ba14","timestamp":"2026-06-04T03:17:25.0Z","cwd":"/home/u/proj","cli_version":"0.137.0","source":"cli"}}
{"timestamp":"2026-06-04T03:17:27.100Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}}
{"timestamp":"2026-06-04T03:17:27.100Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}}
{"timestamp":"2026-06-04T03:17:27.100Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"third"}]}}
`

func TestReconcileOnce_CoTimestampLinesStayOrdered(t *testing.T) {
	codexDir := filepath.Join(t.TempDir(), ".codex")
	t.Setenv("CODEX_HOME", codexDir)
	dir := filepath.Join(codexDir, "sessions", "2026", "06", "04")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "rollout-2026-06-04T03-17-25-019e90a2-c115-7523-b5a3-50c024b3ba14.jsonl")
	if err := os.WriteFile(path, []byte(coTimestampRollout), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ing := newCaptureIngester()
	if err := New().ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	evs := ing.bySession["019e90a2-c115-7523-b5a3-50c024b3ba14"]
	if len(evs) == 0 {
		t.Fatal("no events ingested")
	}

	// Seq must be strictly increasing in capture (file) order.
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq <= evs[i-1].Seq {
			t.Fatalf("Seq not strictly increasing at %d: %d <= %d", i, evs[i].Seq, evs[i-1].Seq)
		}
	}

	// Replay must preserve first→second→third order.
	out, err := New().ToNative(context.Background(), evs, adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("tonative: %v", err)
	}
	s := string(out)
	fi, si, ti := strings.Index(s, "first"), strings.Index(s, "second"), strings.Index(s, "third")
	if fi < 0 || si < 0 || ti < 0 {
		t.Fatalf("replay missing a message: first=%d second=%d third=%d", fi, si, ti)
	}
	if fi >= si || si >= ti {
		t.Fatalf("replay reordered co-timestamped lines: first=%d second=%d third=%d", fi, si, ti)
	}
}

func TestReconcileOnce_IngestsRolloutTree(t *testing.T) {
	codexDir := filepath.Join(t.TempDir(), ".codex")
	t.Setenv("CODEX_HOME", codexDir)
	writeSampleRollout(t, codexDir)

	ing := newCaptureIngester()
	if err := New().ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	const sid = "019e90a2-c115-7523-b5a3-50c024b3ba14"
	evs := ing.bySession[sid]
	if len(evs) == 0 {
		t.Fatalf("no events ingested for %s; sessions seen: %v", sid, keys(ing.bySession))
	}

	kinds := map[event.Kind]int{}
	var model string
	for _, e := range evs {
		kinds[e.Kind]++
		if e.SessionID != sid {
			t.Errorf("event has wrong session id: %q", e.SessionID)
		}
		if e.Kind == event.KindSystem {
			var c map[string]any
			_ = json.Unmarshal(e.Content, &c)
			if m, ok := c["model"].(string); ok && m != "" {
				model = m
			}
		}
	}
	if kinds[event.KindSessionStart] != 1 {
		t.Errorf("want 1 session_start, got %d", kinds[event.KindSessionStart])
	}
	if kinds[event.KindToolCall] != 1 || kinds[event.KindToolResult] != 1 {
		t.Errorf("want 1 tool_call + 1 tool_result, got call=%d result=%d", kinds[event.KindToolCall], kinds[event.KindToolResult])
	}
	// The prompt "do the thing" appears in BOTH a response_item user
	// message and an event_msg user_message. Exactly ONE KindUserMessage
	// must be emitted (canonical event_msg form); the response_item user
	// form is captured as KindSystem, not as a second user message.
	if kinds[event.KindUserMessage] != 1 {
		t.Errorf("want exactly 1 user message (no double-ingest), got %d", kinds[event.KindUserMessage])
	}
	if kinds[event.KindAssistantMessage] < 1 {
		t.Errorf("want >=1 assistant message, got %d", kinds[event.KindAssistantMessage])
	}
	// Verify the response_item user form is preserved as a system record.
	var sawUserSystem bool
	for _, e := range evs {
		if e.Kind != event.KindSystem {
			continue
		}
		var c map[string]any
		_ = json.Unmarshal(e.Content, &c)
		if c["resleeve.system.kind"] == "message:user" && c["text"] == "do the thing" {
			sawUserSystem = true
		}
	}
	if !sawUserSystem {
		t.Errorf("response_item user message should be preserved as a system record")
	}
	if model != "gpt-5-codex" {
		t.Errorf("model not recovered from turn_context: %q", model)
	}
}

// TestReconcileThenToNative_RoundTrip integration: reconcile a rollout
// file, then ToNative the captured events and confirm the native payload
// lines are preserved byte-for-byte (where payload present).
func TestReconcileThenToNative_RoundTrip(t *testing.T) {
	codexDir := filepath.Join(t.TempDir(), ".codex")
	t.Setenv("CODEX_HOME", codexDir)
	writeSampleRollout(t, codexDir)

	ing := newCaptureIngester()
	if err := New().ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	const sid = "019e90a2-c115-7523-b5a3-50c024b3ba14"
	out, err := New().ToNative(context.Background(), ing.bySession[sid], adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative: %v", err)
	}
	got := string(out)
	// Every line that had a native payload should appear verbatim. We
	// check a couple of distinctive original lines round-trip exactly.
	originals := strings.Split(strings.TrimRight(sampleRollout, "\n"), "\n")
	for _, orig := range originals {
		// token_count event_msg is dropped (no event), so skip it.
		if strings.Contains(orig, "token_count") {
			if strings.Contains(got, orig) {
				t.Errorf("dropped event unexpectedly present: %s", orig)
			}
			continue
		}
		if !strings.Contains(got, orig) {
			t.Errorf("native payload line not preserved byte-for-byte:\n%s", orig)
		}
	}
}

func TestSessionIDFromFilename(t *testing.T) {
	cases := map[string]string{
		"rollout-2026-06-04T03-17-25-019e90a2-c115-7523-b5a3-50c024b3ba14.jsonl": "019e90a2-c115-7523-b5a3-50c024b3ba14",
		"rollout-bad.jsonl": "",
		"notarollout.txt":   "",
	}
	for in, want := range cases {
		if got := sessionIDFromFilename(in); got != want {
			t.Errorf("sessionIDFromFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReconcileOnce_MissingTreeNoError(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "does-not-exist"))
	if err := New().ReconcileOnce(context.Background(), newCaptureIngester()); err != nil {
		t.Fatalf("ReconcileOnce on missing tree should not error: %v", err)
	}
}

func keys(m map[string][]event.Event) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
