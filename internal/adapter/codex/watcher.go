package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
// events. The local daemon's *agent.Daemon satisfies it via IngestBatch.
type Ingester interface {
	IngestBatch(ctx context.Context, sessionID string, events []event.Event) error
}

// ReconcileOnce walks $CODEX_HOME/sessions/<YYYY>/<MM>/<DD>/rollout-*.jsonl
// and re-ingests every rollout line through the JSONL path. Deterministic
// UUIDs + INSERT OR IGNORE on (session_id, event_uuid) make this safe
// against events the live hook path already captured.
func (a *Adapter) ReconcileOnce(ctx context.Context, ing Ingester) error {
	root := filepath.Join(codexHome(), "sessions")

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // sessions dir doesn't exist yet — fine
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, ".jsonl") {
			return nil
		}
		if err := a.ingestFile(ctx, ing, path); err != nil {
			log.Printf("codex reconcile %s: %v", path, err)
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
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // up to 8 MiB per line

	const flushSize = 100

	// The session id lives only on the first session_meta line, so we
	// resolve it from the filename up front as a fallback and refine it
	// from the in-file session_meta (the authoritative source — renames
	// are safe). Every subsequent line is decoded with the resolved id
	// passed as the SessionHint.
	sessionID := sessionIDFromFilename(path)
	var (
		batch    []event.Event
		started  bool
		firstCwd string
		firstVer string
		firstTS  time.Time
		// lastSeq enforces a strictly-increasing Seq in file order across
		// the whole file (it must persist across flushes). Codex rollout
		// timestamps are only millisecond-resolution, so multiple lines
		// routinely share ts.UnixNano(); without this, ToNative's
		// (Seq, EventUUID) sort would tie-break co-timestamped lines by
		// their (order-independent) deterministic UUID and reorder them,
		// breaking replay byte-equivalence. Anchoring to the timestamp but
		// bumping on collision keeps Seq ~chronological AND order-stable.
		lastSeq int64
	)

	flush := func() {
		if sessionID == "" || len(batch) == 0 {
			return
		}
		if err := ing.IngestBatch(ctx, sessionID, batch); err != nil {
			log.Printf("codex reconcile ingest (%s): %v", sessionID, err)
		}
		batch = batch[:0]
	}

	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)

		// Peek at the line type first so we can lock onto the in-file
		// session id from session_meta before decoding subsequent lines.
		rl, perr := ParseRolloutLine(line)
		if perr != nil || rl.Type == "" {
			continue
		}
		if rl.Type == "session_meta" {
			var meta SessionMetaPayload
			if json.Unmarshal(rl.Payload, &meta) == nil && meta.ID != "" {
				sessionID = meta.ID
				firstCwd = meta.Cwd
				firstVer = meta.CLIVersion
			}
		}

		events, err := a.FromNative(ctx, line, adapter.Source{
			Kind:        adapter.SourceJSONL,
			SessionHint: sessionID,
		})
		if err != nil || len(events) == 0 {
			continue
		}

		if firstTS.IsZero() {
			firstTS = events[0].Timestamp
		}

		// A real session_meta event already carries KindSessionStart, so
		// the synthesized fallback is only for files that somehow lack one.
		if events[0].Kind == event.KindSessionStart {
			started = true
		}

		// Make Seq strictly increasing in file (and within-line) order.
		// events are already in the order they should replay; this just
		// breaks ms-resolution ties deterministically by position.
		for i := range events {
			if events[i].Seq <= lastSeq {
				events[i].Seq = lastSeq + 1
			}
			lastSeq = events[i].Seq
		}

		batch = append(batch, events...)
		if len(batch) >= flushSize {
			flush()
		}
	}

	// If the file never produced a session_start (no leading session_meta)
	// but we still resolved an id, prepend a synthesized start so the
	// session row learns its cwd. Deterministic key dedups against any
	// live hook capture.
	if !started && sessionID != "" && len(batch) > 0 {
		if firstTS.IsZero() {
			firstTS = time.Now().UTC()
		}
		start := synthesizeSessionStart(sessionID, firstCwd, firstVer, firstTS)
		// Sort ahead of the first real line (which has already been
		// monotonic-normalized above).
		start.Seq = batch[0].Seq - 1
		batch = append([]event.Event{start}, batch...)
	}

	flush()
	return sc.Err()
}

// sessionIDFromFilename extracts the trailing UUID from a rollout
// filename of the form rollout-<ts>-<uuid>.jsonl. The timestamp is
// YYYY-MM-DDThh-mm-ss (fixed shape), so the UUID is the suffix after it.
// Returns "" if the filename doesn't match.
func sessionIDFromFilename(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".jsonl")
	base = strings.TrimPrefix(base, "rollout-")
	// base is now "<YYYY-MM-DDThh-mm-ss>-<uuid>". The UUID has 5 groups
	// joined by '-' (8-4-4-4-12). Split and take the last 5 fields.
	parts := strings.Split(base, "-")
	if len(parts) < 5 {
		return ""
	}
	uuid := strings.Join(parts[len(parts)-5:], "-")
	if !looksLikeUUID(uuid) {
		return ""
	}
	return uuid
}

func looksLikeUUID(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	want := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != want[i] {
			return false
		}
		for _, c := range p {
			if !isHexDigit(c) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
