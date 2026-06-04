package common

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// fixedTime returns a stable RFC3339 timestamp for deterministic test
// rendering.
func fixedTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse fixed time %q: %v", s, err)
	}
	return tt
}

// mustJSON marshals v or fails the test. Returns json.RawMessage so it
// drops directly into Event.Content.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// session returns a populated SessionView used by most cases. Tests can
// mutate fields per-case before passing in.
func session() adapter.SessionView {
	return adapter.SessionView{
		SessionID:  "sess-test-001",
		CLI:        "claude",
		CLIVersion: "1.2.3",
		Cwd:        "/home/op/dev/resleeve",
		GitBranch:  "main",
		Model:      "claude-opus-4-7",
		StartedAt:  time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	}
}

// TestSynthesizePrime drives SynthesizePrime through the renderer's
// branching: empty events, single user message, multi-tool flow with
// tool_call + matched tool_result, plan content inclusion, and
// session/scope context inclusion. The assertions key on the rendered
// markdown's substring shape (not byte-exact output) so harmless
// formatting tweaks don't churn the test.
func TestSynthesizePrime(t *testing.T) {
	type wantContains struct {
		// must are substrings that MUST appear in the rendered output.
		must []string
		// mustNot are substrings that must NOT appear.
		mustNot []string
	}
	tests := []struct {
		name    string
		session adapter.SessionView
		events  []event.Event
		opts    PrimeOpts
		want    wantContains
	}{
		{
			name:    "empty events",
			session: session(),
			events:  nil,
			opts:    PrimeOpts{},
			want: wantContains{
				must: []string{
					"# Resumed session: sess-test-001",
					"## Context",
					"- Source CLI: claude 1.2.3",
					"- Working directory: /home/op/dev/resleeve",
					"- Git branch: main",
					"- Model: claude-opus-4-7",
					"## Plan",
					"(none captured)",
					"## Recent activity",
					"(no recent activity)",
					"## Next",
					"(no open prompt or tool result",
				},
			},
		},
		{
			name:    "single user message",
			session: session(),
			events: []event.Event{
				{
					EventUUID: "u-1",
					Seq:       1,
					Timestamp: fixedTime(t, "2026-06-04T12:00:01Z"),
					Kind:      event.KindUserMessage,
					Content: mustJSON(t, map[string]string{
						"text": "help me refactor prime.go",
					}),
				},
			},
			opts: PrimeOpts{},
			want: wantContains{
				must: []string{
					"user: help me refactor prime.go",
					"## Next",
					"Last user message",
					"> help me refactor prime.go",
				},
				mustNot: []string{
					"(no recent activity)",
				},
			},
		},
		{
			name:    "multi-tool flow with tool_call and tool_result",
			session: session(),
			events: []event.Event{
				{
					EventUUID: "u-1",
					Seq:       1,
					Timestamp: fixedTime(t, "2026-06-04T12:00:01Z"),
					Kind:      event.KindUserMessage,
					Content:   mustJSON(t, map[string]string{"text": "list files"}),
				},
				{
					EventUUID: "a-1",
					Seq:       2,
					Timestamp: fixedTime(t, "2026-06-04T12:00:02Z"),
					Kind:      event.KindAssistantMessage,
					Content:   mustJSON(t, map[string]string{"text": "I'll list them now."}),
				},
				{
					EventUUID: "tc-1",
					Seq:       3,
					Timestamp: fixedTime(t, "2026-06-04T12:00:03Z"),
					Kind:      event.KindToolCall,
					Content: mustJSON(t, map[string]any{
						"tool": map[string]any{
							"id":   "call-42",
							"name": "Bash",
							"args": map[string]string{"command": "ls"},
						},
					}),
				},
				{
					EventUUID: "tr-1",
					Seq:       4,
					Timestamp: fixedTime(t, "2026-06-04T12:00:04Z"),
					Kind:      event.KindToolResult,
					Content: mustJSON(t, map[string]any{
						"tool": map[string]any{
							"call_id": "call-42",
							"status":  "ok",
							"result":  "prime.go prime_test.go",
						},
					}),
				},
				{
					// open call with no result → "(no result captured)"
					EventUUID: "tc-2",
					Seq:       5,
					Timestamp: fixedTime(t, "2026-06-04T12:00:05Z"),
					Kind:      event.KindToolCall,
					Content: mustJSON(t, map[string]any{
						"tool": map[string]any{
							"id":   "call-99",
							"name": "Read",
							"args": map[string]string{"path": "prime.go"},
						},
					}),
				},
			},
			opts: PrimeOpts{},
			want: wantContains{
				must: []string{
					"called Bash(",
					"prime.go prime_test.go",
					"called Read(",
					"(no result captured)",
					// pickNext prefers the last user_message even when
					// open tool calls exist later.
					"Last user message",
					"> list files",
				},
				mustNot: []string{
					// thinking + tool_result are folded/dropped; we should
					// not see a bare tool_result entry on its own line.
					"tool_result",
				},
			},
		},
		{
			name:    "open tool call surfaces as Next when no user_message exists",
			session: session(),
			events: []event.Event{
				{
					EventUUID: "tc-1",
					Seq:       1,
					Timestamp: fixedTime(t, "2026-06-04T12:00:01Z"),
					Kind:      event.KindToolCall,
					Content: mustJSON(t, map[string]any{
						"tool": map[string]any{
							"id":   "call-1",
							"name": "Read",
							"args": map[string]string{"path": "prime.go"},
						},
					}),
				},
			},
			opts: PrimeOpts{},
			want: wantContains{
				must: []string{
					"Open tool call awaiting result: Read(",
				},
			},
		},
		{
			name:    "plan content inclusion",
			session: session(),
			events:  nil,
			opts: PrimeOpts{
				PlanContent: "- ship Q7 tests\n- commit cleanly",
			},
			want: wantContains{
				must: []string{
					"## Plan",
					"- ship Q7 tests",
					"- commit cleanly",
				},
				mustNot: []string{
					"(none captured)",
				},
			},
		},
		{
			name: "scope/session context inclusion populates Context block",
			session: adapter.SessionView{
				SessionID:  "scoped-sess-77",
				CLI:        "opencode",
				CLIVersion: "0.4.0",
				Cwd:        "/srv/fleet/scope-a",
				GitBranch:  "feat/x",
				Model:      "gpt-9",
				StartedAt:  time.Date(2026, 6, 4, 9, 30, 0, 0, time.UTC),
			},
			events: nil,
			opts:   PrimeOpts{},
			want: wantContains{
				must: []string{
					"# Resumed session: scoped-sess-77",
					"- Source CLI: opencode 0.4.0",
					"- Working directory: /srv/fleet/scope-a",
					"- Git branch: feat/x",
					"- Model: gpt-9",
					"- Started: 2026-06-04T09:30:00Z",
				},
			},
		},
		{
			name: "missing session fields render as (unknown)",
			session: adapter.SessionView{
				// no CLI, no cwd, no branch, no model, zero StartedAt.
				SessionID: "",
			},
			events: nil,
			opts:   PrimeOpts{},
			want: wantContains{
				must: []string{
					"# Resumed session: (unknown)",
					"- Source CLI: (unknown)",
					"- Working directory: (unknown)",
					"- Git branch: (unknown)",
					"- Model: (unknown)",
					"- Started: (unknown)",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := SynthesizePrime(tc.session, tc.events, tc.opts)
			if err != nil {
				t.Fatalf("SynthesizePrime: %v", err)
			}
			got := string(out)
			for _, want := range tc.want.must {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\n---\n%s\n---", want, got)
				}
			}
			for _, bad := range tc.want.mustNot {
				if strings.Contains(got, bad) {
					t.Errorf("output unexpectedly contains %q\n---\n%s\n---", bad, got)
				}
			}
		})
	}
}

// TestSynthesizePrime_TruncatesOldestWhenOverMaxBytes drives the
// length-cap branch: when the rendered output would exceed MaxBytes,
// oldest "Recent activity" entries are dropped and a "(N earlier
// events omitted)" note is emitted.
func TestSynthesizePrime_TruncatesOldestWhenOverMaxBytes(t *testing.T) {
	// 25 user messages, each tagged so we can identify which ones survive.
	var events []event.Event
	for i := 0; i < 25; i++ {
		events = append(events, event.Event{
			EventUUID: "u-" + string(rune('A'+i)),
			Seq:       int64(i + 1),
			Timestamp: time.Date(2026, 6, 4, 12, i, 0, 0, time.UTC),
			Kind:      event.KindUserMessage,
			Content:   mustJSON(t, map[string]string{"text": strings.Repeat("x", 100) + "#" + string(rune('A'+i))}),
		})
	}
	out, err := SynthesizePrime(session(), events, PrimeOpts{
		RecentN:  20, // first 5 dropped by RecentN
		MaxBytes: 1500,
	})
	if err != nil {
		t.Fatalf("SynthesizePrime: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "earlier events omitted") {
		t.Errorf("expected truncation note in output\n---\n%s\n---", got)
	}
	if len(out) > 1500+200 /* tail-truncation slack */ {
		t.Errorf("output %d bytes exceeds max+slack 1700", len(out))
	}
}

// TestSynthesizePrime_DropsThinkingAndSessionFraming asserts that
// thinking + session_start/session_end events never reach the rendered
// output, regardless of how many of them exist.
func TestSynthesizePrime_DropsThinkingAndSessionFraming(t *testing.T) {
	events := []event.Event{
		{
			EventUUID: "ss-1",
			Seq:       1,
			Timestamp: fixedTime(t, "2026-06-04T12:00:00Z"),
			Kind:      event.KindSessionStart,
			Content:   mustJSON(t, map[string]any{}),
		},
		{
			EventUUID: "th-1",
			Seq:       2,
			Timestamp: fixedTime(t, "2026-06-04T12:00:01Z"),
			Kind:      event.KindThinking,
			Content:   mustJSON(t, map[string]string{"text": "SECRET_THOUGHT_MARKER"}),
		},
		{
			EventUUID: "u-1",
			Seq:       3,
			Timestamp: fixedTime(t, "2026-06-04T12:00:02Z"),
			Kind:      event.KindUserMessage,
			Content:   mustJSON(t, map[string]string{"text": "visible-user-line"}),
		},
		{
			EventUUID: "se-1",
			Seq:       4,
			Timestamp: fixedTime(t, "2026-06-04T12:00:03Z"),
			Kind:      event.KindSessionEnd,
			Content:   mustJSON(t, map[string]any{}),
		},
	}
	out, err := SynthesizePrime(session(), events, PrimeOpts{})
	if err != nil {
		t.Fatalf("SynthesizePrime: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "SECRET_THOUGHT_MARKER") {
		t.Errorf("thinking event leaked into prime output:\n%s", got)
	}
	if !strings.Contains(got, "visible-user-line") {
		t.Errorf("expected visible user message to survive filter:\n%s", got)
	}
}
