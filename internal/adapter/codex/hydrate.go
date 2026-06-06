package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// Hydrate materializes Codex's local state for a session so that exec'ing
// the returned NativeResumeCmd picks up the session natively.
//
// Replay mode (auto default for codex→codex): writes a rollout JSONL to
//
//	$CODEX_HOME/sessions/<YYYY>/<MM>/<DD>/rollout-<startedAt>-<sessionID>.jsonl
//
// where <startedAt> is the session StartedAt formatted YYYY-MM-DDThh-mm-ss
// (filesystem-safe `-` for `:`). `codex resume <uuid>` discovers this file
// by walking the sessions tree and matching the UUID out of the filename
// (verified rollout/src/list.rs find_rollout_path_by_id_from_filenames).
// Hard requirements honored: filename UUID present; first line a valid
// session_meta whose id equals that UUID; every line valid RolloutLine
// JSON.
//
// Prime mode (opt-in, or forced when source/target CLIs differ): writes a
// synthesized markdown opening prompt to ~/.resleeve/hydrate/<new-uuid>.md
// and mints a fresh session id.
func (a *Adapter) Hydrate(ctx context.Context, session adapter.SessionView, opts adapter.HydrateOpts) (adapter.HydrateResult, error) {
	mode := opts.Mode
	if mode == adapter.RenderModeAuto {
		mode = adapter.RenderModeReplay
	}
	if session.EventStream == nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: session view has no EventStream")
	}
	events, err := session.EventStream()
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: load events: %w", err)
	}

	if mode == adapter.RenderModePrime {
		return a.hydratePrime(ctx, session, events, opts)
	}
	if mode != adapter.RenderModeReplay {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: unknown mode %q", mode)
	}

	if session.SessionID == "" {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: session has no SessionID; can't derive rollout filename")
	}

	body, err := a.ToNative(ctx, events, mode)
	if err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: ToNative: %w", err)
	}

	cwd := opts.Cwd
	if cwd == "" {
		cwd = session.Cwd
	}

	// Guarantee the hard requirement: the first line MUST be a valid
	// session_meta whose id == sessionID. If the replay output doesn't
	// already lead with such a line, prepend a synthesized one.
	body = ensureLeadingSessionMeta(body, session, cwd)

	home := codexHome()
	day := session.StartedAt.UTC()
	dir := filepath.Join(home, "sessions",
		fmt.Sprintf("%04d", day.Year()),
		fmt.Sprintf("%02d", int(day.Month())),
		fmt.Sprintf("%02d", day.Day()),
	)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: mkdir %s: %w", dir, err)
	}
	stamp := session.StartedAt.UTC().Format("2006-01-02T15-04-05")
	out := filepath.Join(dir, fmt.Sprintf("rollout-%s-%s.jsonl", stamp, session.SessionID))

	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: write tempfile: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return adapter.HydrateResult{}, fmt.Errorf("codex.Hydrate: rename: %w", err)
	}

	notes := []string{}
	withPayload := 0
	for _, e := range events {
		if len(e.Vendor.NativePayload) > 0 {
			withPayload++
		}
	}
	if skipped := len(events) - withPayload; skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d event(s) without native payload were synthesized or skipped", skipped))
	}

	return adapter.HydrateResult{
		Mode:      adapter.RenderModeReplay,
		Path:      out,
		SessionID: session.SessionID,
		Notes:     notes,
	}, nil
}

// ensureLeadingSessionMeta guarantees the rollout body begins with a valid
// session_meta line whose payload.id equals session.SessionID. If the
// existing first line already satisfies this, body is returned unchanged;
// otherwise a synthesized session_meta line is prepended. This is the
// invariant `codex resume`'s read-repair asserts (list.rs).
func ensureLeadingSessionMeta(body []byte, session adapter.SessionView, cwd string) []byte {
	if firstLineIsMatchingSessionMeta(body, session.SessionID) {
		return body
	}
	meta := synthesizeSessionMetaLine(session, cwd)
	var buf bytes.Buffer
	buf.Write(meta)
	buf.WriteByte('\n')
	buf.Write(body)
	return buf.Bytes()
}

func firstLineIsMatchingSessionMeta(body []byte, sessionID string) bool {
	nl := bytes.IndexByte(body, '\n')
	first := body
	if nl >= 0 {
		first = body[:nl]
	}
	first = bytes.TrimSpace(first)
	if len(first) == 0 {
		return false
	}
	rl, err := ParseRolloutLine(first)
	if err != nil || rl.Type != "session_meta" {
		return false
	}
	var meta SessionMetaPayload
	if err := json.Unmarshal(rl.Payload, &meta); err != nil {
		return false
	}
	return meta.ID == sessionID
}

func synthesizeSessionMetaLine(session adapter.SessionView, cwd string) json.RawMessage {
	ts := session.StartedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	payload := map[string]any{
		"id":          session.SessionID,
		"timestamp":   ts,
		"cwd":         cwd,
		"cli_version": session.CLIVersion,
		"originator":  "resleeve",
		"source":      "cli",
	}
	if session.GitBranch != "" {
		payload["git"] = map[string]any{"branch": session.GitBranch}
	}
	return marshalRolloutLine(ts, "session_meta", payload)
}
