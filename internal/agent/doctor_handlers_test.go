package agent

import (
	"testing"
	"time"
)

// TestSyncStats_RecordAndSnapshot exercises the in-process stats
// tracking that backs /v1/doctor/sync-status. We assert that
// Snapshot returns a consistent view across drain/pull/sse fields
// and that zero-value times round-trip as zero (the CLI side
// renders zero as "never").
func TestSyncStats_RecordAndSnapshot(t *testing.T) {
	sc := NewSyncClient(nil, "http://upstream.example", "tok")

	// Before any record: snapshot has zero times, empty pull map,
	// sse disconnected.
	snap := sc.Snapshot()
	if !snap.DrainLast.IsZero() {
		t.Errorf("DrainLast not zero before record: %v", snap.DrainLast)
	}
	if snap.SSEConnected {
		t.Errorf("SSEConnected true before any record")
	}
	if snap.PullLastPerKind != nil && len(snap.PullLastPerKind) != 0 {
		t.Errorf("PullLastPerKind non-empty before record: %+v", snap.PullLastPerKind)
	}

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	sc.stats.recordDrain(now)
	sc.stats.recordPull("events", now.Add(time.Second))
	sc.stats.recordPull("memory", now.Add(2*time.Second))
	sc.stats.recordSSEConnected(now.Add(3 * time.Second))
	sc.stats.recordSSEEvent(now.Add(4 * time.Second))

	snap = sc.Snapshot()
	if !snap.DrainLast.Equal(now) {
		t.Errorf("DrainLast = %v, want %v", snap.DrainLast, now)
	}
	if got := snap.PullLastPerKind["events"]; !got.Equal(now.Add(time.Second)) {
		t.Errorf("pull[events] = %v", got)
	}
	if got := snap.PullLastPerKind["memory"]; !got.Equal(now.Add(2 * time.Second)) {
		t.Errorf("pull[memory] = %v", got)
	}
	if !snap.SSEConnected {
		t.Errorf("SSEConnected false after recordSSEConnected")
	}
	if !snap.SSEConnectedAt.Equal(now.Add(3 * time.Second)) {
		t.Errorf("SSEConnectedAt = %v", snap.SSEConnectedAt)
	}
	if !snap.SSELastEventAt.Equal(now.Add(4 * time.Second)) {
		t.Errorf("SSELastEventAt = %v", snap.SSELastEventAt)
	}

	// Disconnect: SSEConnected flips, last event preserved.
	sc.stats.recordSSEDisconnected()
	snap = sc.Snapshot()
	if snap.SSEConnected {
		t.Errorf("SSEConnected still true after disconnect")
	}
	if snap.SSEUptimeSec != 0 {
		t.Errorf("uptime should be 0 when disconnected, got %d", snap.SSEUptimeSec)
	}
	if !snap.SSELastEventAt.Equal(now.Add(4 * time.Second)) {
		t.Errorf("SSELastEventAt cleared on disconnect: %v", snap.SSELastEventAt)
	}
}

// TestSyncStats_SnapshotIsCopy guards against aliasing — the snapshot
// must own its PullLastPerKind map so a caller can't race the
// drain/pull loops by mutating it.
func TestSyncStats_SnapshotIsCopy(t *testing.T) {
	sc := NewSyncClient(nil, "http://upstream", "")
	sc.stats.recordPull("events", time.Now())
	snap := sc.Snapshot()
	// Mutate snapshot's map; original should be untouched.
	snap.PullLastPerKind["events"] = time.Time{}
	snap2 := sc.Snapshot()
	if snap2.PullLastPerKind["events"].IsZero() {
		t.Errorf("internal pullLastPerKind was aliased — caller mutation leaked")
	}
}

// TestSyncClient_UpstreamAccessors covers the trivial getters used
// by the doctor handler.
func TestSyncClient_UpstreamAccessors(t *testing.T) {
	sc := NewSyncClient(nil, "https://sync.example.dev/", "bearer-tok")
	if got := sc.UpstreamURL(); got != "https://sync.example.dev" {
		t.Errorf("UpstreamURL = %q, want trailing slash trimmed", got)
	}
	if got := sc.UpstreamToken(); got != "bearer-tok" {
		t.Errorf("UpstreamToken = %q", got)
	}
}
