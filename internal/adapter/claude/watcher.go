package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/event"
)

// Ingester is the daemon-side interface used by reconcile to commit
// events. The local daemon's *agent.Daemon satisfies it via
// IngestBatch.
type Ingester interface {
	IngestBatch(ctx context.Context, sessionID string, events []event.Event) error
}

// ReconcileOnce walks ~/.claude/projects/**/*.jsonl and re-ingests
// every record through the JSONL path. Deterministic UUIDs +
// INSERT OR IGNORE on (session_id, event_uuid) make this safe against
// events the live hook path already captured.
//
// Stage 3c ships this as a one-shot sweep on daemon startup. A 60s
// ticker for active slots can be added later when live FS watching
// proves insufficient.
func (a *Adapter) ReconcileOnce(ctx context.Context, ing Ingester) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("homedir: %w", err)
	}
	root := filepath.Join(home, ".claude", "projects")

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // projects dir doesn't exist yet — fine
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if err := a.ingestFile(ctx, ing, path); err != nil {
			log.Printf("reconcile %s: %v", path, err)
		}
		return nil
	})
}

func (a *Adapter) ingestFile(ctx context.Context, ing Ingester, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // up to 4 MiB per line

	const flushSize = 100
	// pending buffers events from a session until we have observed a
	// valid (non-empty) cwd. Once observed we synthesize a session_start
	// at the head of the batch, then flush as normal. This is the
	// reconcile-side fix for F1/F2/F3: Claude Code's JSONL stream has
	// no native session_start, and some leading records (e.g.
	// permission-mode) have cwd:null which would otherwise leak
	// scope='unknown' into the sessions row.
	var (
		batch            []event.Event
		pending          []event.Event
		currentSession   string
		sessionCwd       string
		sessionVersion   string
		sessionTimestamp time.Time
		synthesized      bool
	)

	flush := func() {
		if currentSession == "" || len(batch) == 0 {
			return
		}
		if err := ing.IngestBatch(ctx, currentSession, batch); err != nil {
			log.Printf("reconcile ingest (%s): %v", currentSession, err)
		}
		batch = batch[:0]
	}

	// resetSession is called when we encounter a new sessionID in the
	// file. It first flushes whatever is buffered for the prior session
	// (synthesizing a start if we ever found a cwd for it; otherwise
	// flushing pending as-is so events are not lost).
	resetSession := func(newSid string) {
		if currentSession != "" && len(pending) > 0 && !synthesized {
			// We never found a valid cwd for the prior session; flush
			// pending as-is. scope='unknown' is the best we can do.
			batch = append(batch, pending...)
			pending = pending[:0]
		}
		flush()
		currentSession = newSid
		sessionCwd = ""
		sessionVersion = ""
		sessionTimestamp = time.Time{}
		synthesized = false
	}

	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		events, err := a.FromNative(ctx, line, adapter.Source{Kind: adapter.SourceJSONL})
		if err != nil || len(events) == 0 {
			continue
		}
		sid := events[0].SessionID
		if currentSession != "" && currentSession != sid {
			resetSession(sid)
		} else if currentSession == "" {
			currentSession = sid
		}

		// Track first observed timestamp/version for the session so the
		// synthesized start has plausible metadata.
		if sessionTimestamp.IsZero() {
			sessionTimestamp = events[0].Timestamp
		}
		if sessionVersion == "" {
			sessionVersion = events[0].Vendor.Version
		}

		if !synthesized {
			// Look for the first event with a real cwd (Slot.Scope set
			// from a non-empty cwd). Records with cwd:null produce
			// Slot.Scope == "unknown" via deriveScope.
			cwd := extractRecordCwd(line)
			if cwd != "" {
				sessionCwd = cwd
				start := synthesizeSessionStart(currentSession, sessionCwd, sessionVersion, sessionTimestamp)
				batch = append(batch, start)
				// Drain anything we buffered before finding the cwd,
				// patching their slot scope to match.
				for _, e := range pending {
					if e.Slot.Scope == "unknown" {
						e.Slot.Scope = start.Slot.Scope
					}
					batch = append(batch, e)
				}
				pending = pending[:0]
				synthesized = true
			}
		}

		if !synthesized {
			pending = append(pending, events...)
			continue
		}

		// Already synthesized: backfill scope on any record whose own
		// cwd was empty (e.g. tail permission-mode records) so the slot
		// heartbeat stays consistent.
		for i := range events {
			if events[i].Slot.Scope == "unknown" && sessionCwd != "" {
				events[i].Slot.Scope = deriveScope(sessionCwd)
			}
		}
		batch = append(batch, events...)
		if len(batch) >= flushSize {
			flush()
		}
	}

	// End of file: drain pending (no cwd ever found) and flush.
	if len(pending) > 0 {
		batch = append(batch, pending...)
		pending = pending[:0]
	}
	flush()
	return sc.Err()
}

// extractRecordCwd parses just enough of a JSONL line to return its cwd
// field, or "" if missing/null. Cheaper than a full re-parse — we use
// the already-decoded slot scope when possible, but the slot loses the
// distinction between "" and "unknown".
func extractRecordCwd(line []byte) string {
	rec, err := ParseRecord(line)
	if err != nil {
		return ""
	}
	return rec.Cwd
}
