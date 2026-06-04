package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	// when the daemon responds 409 (see internal/agent/sync_handlers.go):
	// a wrapped agent.ErrNoUpstream sentinel.
	p := &fakePuller{fn: func(_ context.Context) (*agent.SyncPullResp, error) {
		return nil, fmt.Errorf("daemon 409 Conflict: no upstream configured — set --upstream on `resleeve up`: %w", agent.ErrNoUpstream)
	}}
	var buf bytes.Buffer
	tryOnDemandPull(context.Background(), p, time.Second, &buf)
	if buf.Len() != 0 {
		t.Fatalf("expected silent fallback for standalone deployment, got %q", buf.String())
	}
}

// TestTryOnDemandPull_TypedSentinelDetected guards the typed-sentinel
// contract directly: any error chain that wraps agent.ErrNoUpstream
// must be treated as "standalone, silently skip", independent of the
// surrounding string formatting. Replaces the pre-followup-F
// strings.Contains coupling.
func TestTryOnDemandPull_TypedSentinelDetected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"bare sentinel", agent.ErrNoUpstream},
		{"wrapped with arbitrary prefix", fmt.Errorf("anything at all: %w", agent.ErrNoUpstream)},
		{"double-wrapped", fmt.Errorf("outer: %w", fmt.Errorf("middle: %w", agent.ErrNoUpstream))},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &fakePuller{fn: func(_ context.Context) (*agent.SyncPullResp, error) {
				return nil, tc.err
			}}
			var buf bytes.Buffer
			tryOnDemandPull(context.Background(), p, time.Second, &buf)
			if buf.Len() != 0 {
				t.Fatalf("expected silent fallback when err wraps ErrNoUpstream, got %q", buf.String())
			}
			if !isNoUpstreamConfigured(tc.err) {
				t.Fatalf("isNoUpstreamConfigured returned false for %v", tc.err)
			}
		})
	}

	// Negative control: a stringly-similar error that does NOT wrap
	// the sentinel must NOT be classified as standalone-mode (this is
	// the exact regression the typed sentinel prevents).
	stringyTwin := errors.New("daemon 409 Conflict: no upstream configured — set --upstream on `resleeve up`")
	if isNoUpstreamConfigured(stringyTwin) {
		t.Fatalf("expected stringy-twin error to NOT match typed sentinel")
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
