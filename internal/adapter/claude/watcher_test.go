package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattkorwel/resleeve/internal/event"
)

// fakeIngester captures the batches passed to IngestBatch so tests can
// assert what events flowed through the reconcile path.
type fakeIngester struct {
	batches [][]event.Event
}

func (f *fakeIngester) IngestBatch(_ context.Context, _ string, events []event.Event) error {
	// Copy to avoid aliasing the watcher's internal buffer (which is reused).
	cp := make([]event.Event, len(events))
	copy(cp, events)
	f.batches = append(f.batches, cp)
	return nil
}

// allEvents flattens batches in order for easier assertions.
func (f *fakeIngester) allEvents() []event.Event {
	var out []event.Event
	for _, b := range f.batches {
		out = append(out, b...)
	}
	return out
}

func writeJSONL(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "session.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	return path
}

// F1+F2+F3: reconcile-only sessions must produce a session_start with a
// non-null cwd, even when the first record carries cwd:null
// (permission-mode case).
func TestIngestFile_SynthesizesSessionStart_SkipsNullCwd(t *testing.T) {
	dir := t.TempDir()
	// First record: permission-mode with cwd:null — should be skipped
	// when searching for the session's true cwd.
	// Second record: user message with cwd:"/x/y/z" — this is where
	// we lock in the synthesized cwd (and scope="z").
	path := writeJSONL(t, dir,
		`{"type":"permission-mode","uuid":"r1","sessionId":"S1","timestamp":"2026-05-12T22:13:04Z","cwd":null,"permissionMode":"default"}`,
		`{"type":"user","uuid":"r2","sessionId":"S1","timestamp":"2026-05-12T22:13:05Z","cwd":"/x/y/z","version":"2.1.140","message":{"role":"user","content":"hello"}}`,
	)

	a := New()
	ing := &fakeIngester{}
	if err := a.ingestFile(context.Background(), ing, path); err != nil {
		t.Fatalf("ingestFile: %v", err)
	}

	events := ing.allEvents()
	if len(events) == 0 {
		t.Fatalf("no events emitted")
	}

	// First event must be the synthesized session_start.
	first := events[0]
	if first.Kind != event.KindSessionStart {
		t.Fatalf("first event kind: got %q, want %q", first.Kind, event.KindSessionStart)
	}
	if first.Slot.Scope != "z" {
		t.Errorf("synthesized session_start scope: got %q, want %q", first.Slot.Scope, "z")
	}

	// session_start content must include cwd:"/x/y/z" so
	// createSessionFromFirstEvent picks it up.
	var c struct {
		Cwd    string `json:"cwd"`
		CLI    string `json:"cli"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(first.Content, &c); err != nil {
		t.Fatalf("unmarshal session_start content: %v", err)
	}
	if c.Cwd != "/x/y/z" {
		t.Errorf("synthesized session_start cwd: got %q, want %q", c.Cwd, "/x/y/z")
	}
	if c.CLI != Name {
		t.Errorf("synthesized session_start cli: got %q, want %q", c.CLI, Name)
	}
	if c.Source != "reconcile" {
		t.Errorf("synthesized session_start source: got %q, want %q", c.Source, "reconcile")
	}

	// The permission-mode record that came before the valid cwd must
	// still be present, and its slot scope should have been patched to
	// match the synthesized session's scope.
	if len(events) < 3 {
		t.Fatalf("expected at least 3 events (synthesized start + permission-mode + user), got %d", len(events))
	}
	if events[1].Kind != event.KindSystem {
		t.Errorf("events[1].Kind: got %q, want %q (permission-mode)", events[1].Kind, event.KindSystem)
	}
	if events[1].Slot.Scope != "z" {
		t.Errorf("events[1].Slot.Scope: got %q, want %q (should be patched from 'unknown')", events[1].Slot.Scope, "z")
	}
}

// Synthesized session_start UUID must match what the live SessionStart
// hook would produce — that's what lets INSERT OR IGNORE dedupe across
// the two capture paths.
func TestIngestFile_SynthesizedStart_UUIDMatchesHook(t *testing.T) {
	dir := t.TempDir()
	path := writeJSONL(t, dir,
		`{"type":"user","uuid":"r1","sessionId":"Sxyz","timestamp":"2026-05-12T22:13:04Z","cwd":"/p","version":"2.1.140","message":{"role":"user","content":"hi"}}`,
	)
	a := New()
	ing := &fakeIngester{}
	if err := a.ingestFile(context.Background(), ing, path); err != nil {
		t.Fatalf("ingestFile: %v", err)
	}
	events := ing.allEvents()
	if len(events) == 0 || events[0].Kind != event.KindSessionStart {
		t.Fatalf("expected first event to be session_start, got %+v", events)
	}
	want := DeterministicEventUUID("Sxyz", "session_start")
	if events[0].EventUUID != want {
		t.Errorf("synthesized session_start UUID: got %q, want %q", events[0].EventUUID, want)
	}
}

// If no record in the file ever carries a non-null cwd, the reconcile
// path should still emit the records it saw — just without a
// synthesized start. scope='unknown' on the resulting session is the
// documented last-resort fallback.
func TestIngestFile_NoCwdAnywhere_NoSynthesis(t *testing.T) {
	dir := t.TempDir()
	path := writeJSONL(t, dir,
		`{"type":"permission-mode","uuid":"r1","sessionId":"S1","timestamp":"2026-05-12T22:13:04Z","cwd":null,"permissionMode":"default"}`,
		`{"type":"permission-mode","uuid":"r2","sessionId":"S1","timestamp":"2026-05-12T22:13:05Z","cwd":null,"permissionMode":"plan"}`,
	)
	a := New()
	ing := &fakeIngester{}
	if err := a.ingestFile(context.Background(), ing, path); err != nil {
		t.Fatalf("ingestFile: %v", err)
	}
	events := ing.allEvents()
	if len(events) == 0 {
		t.Fatalf("expected pending events to be flushed, got none")
	}
	for _, e := range events {
		if e.Kind == event.KindSessionStart {
			t.Fatalf("did not expect a synthesized session_start; got one with scope=%q", e.Slot.Scope)
		}
	}
}
