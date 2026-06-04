package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter/claude"
	rsql "github.com/mattkorwel/resleeve/internal/storage/sql"
)

// BackfillSessionCwd repairs sessions.cwd (and sessions.scope) for
// rows created before the F1+F2+F3 reconcile fix (commit 8a42c22)
// that synthesizes a session_start record. Sessions captured before
// that fix have cwd='' / scope='unknown' because the old reconcile
// path never observed a cwd-bearing record.
//
// Algorithm:
//  1. List sessions whose cwd is empty (LIMIT high — flag-gated, not
//     hot path).
//  2. Locate the source JSONL for each session under
//     ~/.claude/projects/**/<sessionID>.jsonl. We can't predict the
//     project-dir name because cwd was lost; walk + match by filename.
//  3. Stream records, find the first one with a non-null cwd, and
//     UPDATE the row's cwd and scope. Scope uses claude.DeriveScope
//     so the F11 marker walk still applies when the path still
//     exists on disk.
//
// Returns (repaired, skipped). Skipped covers: no JSONL found on
// this machine, or JSONL had no record with a usable cwd.
//
// Caller is responsible for ensuring the daemon is NOT running
// concurrently; the backfill performs raw UPDATEs that bypass the
// daemon's ingest pipeline.
func BackfillSessionCwd(ctx context.Context, store rsql.Store, logf func(string, ...any)) (repaired, skipped int, err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return 0, 0, fmt.Errorf("homedir: %w", err)
	}
	projectsRoot := filepath.Join(home, ".claude", "projects")

	// Build an index: session id -> JSONL path. One walk over the
	// projects tree is O(files); much cheaper than re-walking per
	// session.
	index, err := indexClaudeJSONL(projectsRoot)
	if err != nil {
		return 0, 0, fmt.Errorf("index jsonl: %w", err)
	}

	// Pull sessions with empty cwd. We use List with no filter and
	// the max limit (500), then filter client-side — the
	// SessionFilter doesn't currently expose a cwd predicate, and
	// adding one for this one-shot migration would over-engineer the
	// interface. If a user has >500 sessions to backfill, they can
	// rerun until repaired+skipped == 0.
	rows, err := store.Sessions().List(ctx, rsql.SessionFilter{Limit: 500})
	if err != nil {
		return 0, 0, fmt.Errorf("list sessions: %w", err)
	}

	for _, ses := range rows {
		if ses.Cwd != "" {
			continue
		}
		path, ok := index[ses.ID]
		if !ok {
			logf("skip %s: no source JSONL found under %s\n", ses.ID, projectsRoot)
			skipped++
			continue
		}
		cwd, ferr := firstCwdFromJSONL(path)
		if ferr != nil {
			logf("skip %s: read %s: %v\n", ses.ID, path, ferr)
			skipped++
			continue
		}
		if cwd == "" {
			logf("skip %s: %s had no record with a usable cwd\n", ses.ID, path)
			skipped++
			continue
		}
		scope := claude.DeriveScope(cwd)
		if uerr := store.Sessions().UpdateCwd(ctx, ses.ID, cwd, scope); uerr != nil {
			return repaired, skipped, fmt.Errorf("update %s: %w", ses.ID, uerr)
		}
		logf("repaired %s: cwd=%s scope=%s\n", ses.ID, cwd, scope)
		repaired++
	}
	return repaired, skipped, nil
}

// indexClaudeJSONL walks the Claude Code projects root once and
// returns a map of session id (file basename without .jsonl) -> full
// path. A missing root is not an error — returns an empty map.
func indexClaudeJSONL(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if errors.Is(werr, os.ErrNotExist) {
				return nil
			}
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		// If multiple files exist for the same id across project
		// dirs (a cwd-renamed move would do this), keep the first.
		if _, dup := out[base]; !dup {
			out[base] = path
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	return out, err
}

// firstCwdFromJSONL streams a Claude Code session JSONL and returns
// the first non-empty cwd it observes. Returns "" if no record
// carries one (e.g. file only contains permission-mode records with
// cwd:null).
func firstCwdFromJSONL(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		rec, perr := claude.ParseRecord(line)
		if perr != nil || rec == nil {
			continue
		}
		if rec.Cwd != "" {
			return rec.Cwd, nil
		}
	}
	return "", sc.Err()
}
