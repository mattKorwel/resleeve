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
	var batch []event.Event
	var currentSession string

	flush := func() {
		if currentSession == "" || len(batch) == 0 {
			return
		}
		if err := ing.IngestBatch(ctx, currentSession, batch); err != nil {
			log.Printf("reconcile ingest (%s): %v", currentSession, err)
		}
		batch = batch[:0]
	}

	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		events, err := a.FromNative(ctx, line, adapter.Source{Kind: adapter.SourceJSONL})
		if err != nil || len(events) == 0 {
			continue
		}
		sid := events[0].SessionID
		if currentSession != "" && currentSession != sid {
			flush()
		}
		currentSession = sid
		batch = append(batch, events...)
		if len(batch) >= flushSize {
			flush()
		}
	}
	flush()
	return sc.Err()
}
