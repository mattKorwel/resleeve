package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

func TestToNative_ReplayPrefersNativePayload(t *testing.T) {
	a := New()
	events := []event.Event{
		{
			EventUUID: "E1",
			Seq:       100,
			Kind:      event.KindUserMessage,
			Vendor:    event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"user","sessionId":"S1","cwd":"/x","message":{"role":"user","content":"hi"}}`)},
		},
		{
			EventUUID: "E2",
			Seq:       200,
			Kind:      event.KindAssistantMessage,
			Vendor:    event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"assistant","sessionId":"S1","cwd":"/x","message":{"role":"assistant","content":"hello"}}`)},
		},
	}
	body, err := a.ToNative(context.Background(), events, adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d: %q", len(lines), string(body))
	}
	if !strings.Contains(lines[0], `"type":"user"`) {
		t.Errorf("line 0 should be the user message: %q", lines[0])
	}
	if !strings.Contains(lines[1], `"type":"assistant"`) {
		t.Errorf("line 1 should be the assistant message: %q", lines[1])
	}
}

func TestToNative_ReplaySortsBySeqThenUUID(t *testing.T) {
	a := New()
	// Same Seq — UUID order should determine output ordering.
	events := []event.Event{
		{EventUUID: "B", Seq: 100, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"b"}`)}},
		{EventUUID: "A", Seq: 100, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"a"}`)}},
		{EventUUID: "C", Seq: 50, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"c"}`)}},
	}
	body, err := a.ToNative(context.Background(), events, adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative: %v", err)
	}
	want := `{"type":"c"}` + "\n" + `{"type":"a"}` + "\n" + `{"type":"b"}` + "\n"
	if string(body) != want {
		t.Errorf("ordering wrong:\n got: %q\nwant: %q", string(body), want)
	}
}

func TestToNative_ReplaySkipsEventsWithoutPayload(t *testing.T) {
	a := New()
	events := []event.Event{
		{EventUUID: "S", Seq: 1, Kind: event.KindSessionStart}, // synthesized; no payload
		{EventUUID: "U", Seq: 2, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"user"}`)}},
	}
	body, err := a.ToNative(context.Background(), events, adapter.RenderModeReplay)
	if err != nil {
		t.Fatalf("ToNative: %v", err)
	}
	if strings.Count(string(body), "\n") != 1 {
		t.Errorf("expected 1 line (synthesized event skipped), got %q", string(body))
	}
	if !strings.Contains(string(body), `"type":"user"`) {
		t.Errorf("user event missing from output: %q", string(body))
	}
}

func TestToNative_AutoDefaultsToReplay(t *testing.T) {
	a := New()
	events := []event.Event{
		{EventUUID: "E1", Seq: 1, Vendor: event.Vendor{Name: Name, NativePayload: json.RawMessage(`{"type":"user"}`)}},
	}
	body, err := a.ToNative(context.Background(), events, adapter.RenderModeAuto)
	if err != nil {
		t.Fatalf("ToNative auto: %v", err)
	}
	if !strings.Contains(string(body), `"type":"user"`) {
		t.Errorf("auto mode didn't produce replay output: %q", string(body))
	}
}

func TestToNative_PrimeNotImplemented(t *testing.T) {
	a := New()
	_, err := a.ToNative(context.Background(), nil, adapter.RenderModePrime)
	if err == nil {
		t.Fatal("expected ErrNotImplemented for prime mode")
	}
}
