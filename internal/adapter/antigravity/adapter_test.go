package antigravity

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

func TestName(t *testing.T) {
	if New().Name() != "antigravity" {
		t.Fatalf("name = %q", New().Name())
	}
}

func TestFromNative_Unsupported(t *testing.T) {
	_, err := New().FromNative(context.Background(), []byte("x"), adapter.Source{Kind: adapter.SourceHook})
	if err == nil {
		t.Fatal("FromNative should error (unsupported)")
	}
	if !strings.Contains(err.Error(), "ReconcileOnce") {
		t.Errorf("error should point to ReconcileOnce: %v", err)
	}
}

func TestToNative_ReplayUnsupported(t *testing.T) {
	_, err := New().ToNative(context.Background(), nil, adapter.RenderModeReplay)
	if err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("replay should be unsupported, got: %v", err)
	}
}

func TestToNative_PrimeWorks(t *testing.T) {
	evs := []event.Event{{
		SessionID: "conv-1",
		Kind:      event.KindUserMessage,
		Content:   mustJSON(map[string]any{"text": "hi"}),
	}}
	out, err := New().ToNative(context.Background(), evs, adapter.RenderModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Error("prime output empty")
	}
}

func TestNativeResumeCmd_ReplayIsConversation(t *testing.T) {
	cmd, args, err := New().NativeResumeCmd(context.Background(),
		adapter.SessionView{SessionID: "abc-123"},
		adapter.HydrateResult{Mode: adapter.RenderModeReplay})
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "agy" || len(args) != 2 || args[0] != "--conversation" || args[1] != "abc-123" {
		t.Errorf("cmd=%q args=%v", cmd, args)
	}
}

func TestNativeResumeCmd_Prime(t *testing.T) {
	cmd, args, err := New().NativeResumeCmd(context.Background(),
		adapter.SessionView{},
		adapter.HydrateResult{Mode: adapter.RenderModePrime, Path: "/tmp/x.md"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == "" || len(args) == 0 {
		t.Errorf("prime resume cmd empty: %q %v", cmd, args)
	}
}

func TestInstallBridge_Noop(t *testing.T) {
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{}); err != nil {
		t.Errorf("InstallBridge should be a no-op: %v", err)
	}
	if err := a.UninstallBridge(context.Background()); err != nil {
		t.Errorf("UninstallBridge should be a no-op: %v", err)
	}
}

// captureIngester records ingested events for assertions.
type captureIngester struct {
	mu     sync.Mutex
	bySess map[string][]event.Event
}

func newCaptureIngester() *captureIngester {
	return &captureIngester{bySess: map[string][]event.Event{}}
}

func (c *captureIngester) IngestBatch(ctx context.Context, sessionID string, events []event.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bySess[sessionID] = append(c.bySess[sessionID], events...)
	return nil
}
