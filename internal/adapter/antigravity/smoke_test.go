package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/event"
)

// TestSmoke_RealConversations runs ReconcileOnce against the operator's REAL
// ~/.gemini/antigravity-cli/conversations and asserts we captured at least
// one conversation with recoverable user text. It is guarded behind
// RESLEEVE_SMOKE=1 so it never runs in CI / on machines without agy data —
// the same opt-in pattern other real-data smoke tests use.
//
// Run with:  RESLEEVE_SMOKE=1 go test ./internal/adapter/antigravity/ -run Smoke -v
func TestSmoke_RealConversations(t *testing.T) {
	if os.Getenv("RESLEEVE_SMOKE") != "1" {
		t.Skip("set RESLEEVE_SMOKE=1 to run against real ~/.gemini/antigravity-cli data")
	}

	dir, err := conversationsDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("no conversations dir at %s: %v", dir, err)
	}

	ing := newCaptureIngester()
	if err := New().ReconcileOnce(context.Background(), ing); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	ing.mu.Lock()
	defer ing.mu.Unlock()

	if len(ing.bySess) == 0 {
		t.Fatal("no conversations captured")
	}
	t.Logf("captured %d conversation(s)", len(ing.bySess))

	var (
		totalUser, totalAssistant, totalTool, totalSystem, totalStart int
		foundExpectedPrompt                                           bool
		sampleUserTexts                                               []string
	)
	for sess, evs := range ing.bySess {
		t.Logf("  conversation %s: %d events", sess, len(evs))
		for _, e := range evs {
			switch e.Kind {
			case event.KindSessionStart:
				totalStart++
			case event.KindUserMessage:
				totalUser++
				var c map[string]any
				_ = json.Unmarshal(e.Content, &c)
				txt, _ := c["text"].(string)
				if txt != "" && len(sampleUserTexts) < 5 {
					sampleUserTexts = append(sampleUserTexts, txt)
				}
				if strings.Contains(txt, "parallel and background agents") {
					foundExpectedPrompt = true
				}
			case event.KindAssistantMessage:
				totalAssistant++
			case event.KindToolCall:
				totalTool++
			case event.KindSystem:
				totalSystem++
			}
		}
	}

	t.Logf("kinds: start=%d user=%d assistant=%d tool=%d system=%d",
		totalStart, totalUser, totalAssistant, totalTool, totalSystem)
	for _, s := range sampleUserTexts {
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		t.Logf("  user text: %q", s)
	}

	if totalUser == 0 {
		t.Error("expected at least one user message with recoverable text")
	}
	if !foundExpectedPrompt {
		t.Error(`expected to recover the known prompt "tell me about parallel and background agents..."`)
	}

	// Idempotency: a second sweep must produce identical event UUIDs.
	ing2 := newCaptureIngester()
	if err := New().ReconcileOnce(context.Background(), ing2); err != nil {
		t.Fatalf("second ReconcileOnce: %v", err)
	}
	ing2.mu.Lock()
	defer ing2.mu.Unlock()
	for sess, evs := range ing.bySess {
		got := ing2.bySess[sess]
		if len(got) != len(evs) {
			t.Errorf("conversation %s: event count drift %d -> %d", sess, len(evs), len(got))
			continue
		}
		for i := range evs {
			if evs[i].EventUUID != got[i].EventUUID {
				t.Errorf("conversation %s event %d: UUID drift %s -> %s", sess, i, evs[i].EventUUID, got[i].EventUUID)
			}
		}
	}
}
