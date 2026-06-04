package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattkorwel/resleeve/internal/agent"
)

// fakePuller is a syncPuller stub used by the helper tests. It records
// invocation count and lets each test inject the response/error pair.
type fakePuller struct {
	calls atomic.Int32
	fn    func(ctx context.Context) (*agent.SyncPullResp, error)
}

func (f *fakePuller) SyncPullNow(ctx context.Context) (*agent.SyncPullResp, error) {
	f.calls.Add(1)
	return f.fn(ctx)
}

func TestTryOnDemandPull_SuccessIsSilent(t *testing.T) {
	t.Parallel()
	p := &fakePuller{fn: func(_ context.Context) (*agent.SyncPullResp, error) {
		return &agent.SyncPullResp{Pulled: map[string]int{"sessions": 1}}, nil
	}}
	var buf bytes.Buffer
	tryOnDemandPull(context.Background(), p, time.Second, &buf)
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("expected SyncPullNow called once, got %d", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no warning on success, got %q", buf.String())
	}
}

func TestTryOnDemandPull_NetworkErrorWarnsAndContinues(t *testing.T) {
	t.Parallel()
	p := &fakePuller{fn: func(_ context.Context) (*agent.SyncPullResp, error) {
		return nil, errors.New("POST http://127.0.0.1:9/v1/sync/pull-now: connection refused")
	}}
	var buf bytes.Buffer
	tryOnDemandPull(context.Background(), p, time.Second, &buf)
	if !strings.Contains(buf.String(), "upstream unreachable") {
		t.Fatalf("expected unreachable warning, got %q", buf.String())
	}
}

func TestTryOnDemandPull_NoUpstreamIsSilent(t *testing.T) {
	t.Parallel()
	// Mirrors the wire-format error returned by agent.Client.doJSON
	// when the daemon responds 409 (see internal/agent/sync_handlers.go).
	p := &fakePuller{fn: func(_ context.Context) (*agent.SyncPullResp, error) {
		return nil, errors.New("daemon 409 Conflict: no upstream configured — set --upstream on `resleeve up`")
	}}
	var buf bytes.Buffer
	tryOnDemandPull(context.Background(), p, time.Second, &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected silent fallback for standalone deployment, got %q", buf.String())
	}
}

func TestTryOnDemandPull_NilClientIsNoop(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tryOnDemandPull(context.Background(), nil, time.Second, &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected silent no-op for nil client, got %q", buf.String())
	}
}

func TestTryOnDemandPull_TimeoutWarnsAndContinues(t *testing.T) {
	t.Parallel()
	// Block until the helper's ctx is canceled by the timeout, then
	// surface ctx.Err() — same shape as a real slow upstream that
	// trips the 2s cap.
	p := &fakePuller{fn: func(ctx context.Context) (*agent.SyncPullResp, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	var buf bytes.Buffer
	start := time.Now()
	tryOnDemandPull(context.Background(), p, 25*time.Millisecond, &buf)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("helper did not honor timeout: %s", elapsed)
	}
	if !strings.Contains(buf.String(), "upstream unreachable") {
		t.Fatalf("expected unreachable warning on timeout, got %q", buf.String())
	}
}
