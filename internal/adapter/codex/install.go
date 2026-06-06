package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// hookEventsToInstall is the set of Codex hook events resleeve wants to
// capture. PreToolUse / PermissionRequest / compaction events are skipped
// for v1 (no captured state). Codex silently tolerates unknown event keys
// so the full set is forward-safe.
var hookEventsToInstall = []string{
	"SessionStart",
	"PostToolUse",
	"Stop",
	"UserPromptSubmit",
}

// Hook groups in $CODEX_HOME/hooks.json use a matcher-wrapped shape
// (verified config/src/hook_config.rs):
//
//	{ "hooks": {
//	    "SessionStart": [ { "hooks": [ { "type": "command",
//	                        "command": "resleeve hook --adapter codex" } ] } ]
//	} }
//
// IMPORTANT: Codex has no durable per-hook id (discovery.rs TODO) and
// hooks are untrusted by default — they are discovered but NOT executed
// until the user trusts them once via the TUI. So resleeve OWNS this file
// wholesale: read → merge our block → write. We never surgically edit a
// shared config file.
//
// We identify our OWN handlers by their command string (it contains the
// resleeveHookCommandMarker, "hook --adapter codex"), NOT by injecting a
// non-schema marker field. The handler config in hook_config.rs is a
// `#[serde(tag = "type")]` enum (HookHandlerConfig::Command) whose schemars
// JSON schema is validated; a stray field like `_resleeve` risks being
// rejected and silently disabling ALL hooks (including the user's), so we
// avoid it entirely. Matching by command string still lets us preserve
// user-authored hook groups across install/uninstall.

// InstallBridge writes resleeve's hook entries to $CODEX_HOME/hooks.json.
// Idempotent: re-installation strips prior resleeve groups (recognized by
// their command string) and re-appends a fresh one; unrelated user-authored
// hook groups are preserved untouched.
//
// NOTE: an auto-installed Codex hook is INERT until the user trusts it
// once via the TUI review (hooks are untrusted by default). Callers
// (install-bridge / doctor) should surface this to the user.
func (a *Adapter) InstallBridge(ctx context.Context, opts adapter.InstallOpts) error {
	binPath, err := resolveBinPath(opts.ResleeveBinPath)
	if err != nil {
		return err
	}

	path := hooksPath()
	file, err := readHooksFile(path)
	if err != nil {
		return err
	}

	hooks, _ := file["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		file["hooks"] = hooks
	}

	handler := makeResleeveHandler(binPath, opts.MemoryOnly)

	// Memory-only mode installs ONLY SessionStart (for additionalContext
	// injection); capture for tool calls / prompts / session end is off.
	eventsToInstall := hookEventsToInstall
	if opts.MemoryOnly {
		eventsToInstall = []string{"SessionStart"}
		// Strip prior resleeve groups from the events we're NOT
		// re-installing, so a full → memory-only switch leaves no stale
		// capturing hooks behind.
		for _, eventName := range hookEventsToInstall {
			if eventName == "SessionStart" {
				continue
			}
			if existing, ok := hooks[eventName].([]any); ok {
				stripped := stripResleeveFromEvent(existing)
				if len(stripped) == 0 {
					delete(hooks, eventName)
				} else {
					hooks[eventName] = stripped
				}
			}
		}
	}

	for _, eventName := range eventsToInstall {
		existing, _ := hooks[eventName].([]any)
		stripped := stripResleeveFromEvent(existing)
		ourGroup := map[string]any{
			"hooks": []any{handler},
		}
		hooks[eventName] = append(stripped, ourGroup)
	}

	if opts.DryRun {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(file)
	}

	return writeHooksFile(path, file)
}

// UninstallBridge removes resleeve's hook groups from hooks.json,
// preserving user-authored ones. If the file ends up with no hooks at
// all (we owned everything), it is removed entirely.
func (a *Adapter) UninstallBridge(ctx context.Context) error {
	path := hooksPath()
	file, err := readHooksFile(path)
	if err != nil {
		return err
	}
	hooks, ok := file["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	for eventName, raw := range hooks {
		handlers, ok := raw.([]any)
		if !ok {
			continue
		}
		kept := stripResleeveFromEvent(handlers)
		if len(kept) == 0 {
			delete(hooks, eventName)
		} else {
			hooks[eventName] = kept
		}
	}
	if len(hooks) == 0 {
		delete(file, "hooks")
	}
	// If the file is now empty (no hooks, no other keys), remove it —
	// resleeve owns this file, so an empty owned file should not linger.
	if len(file) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	return writeHooksFile(path, file)
}

// resleeveHookCommandMarker is the stable substring that identifies a
// resleeve-installed command handler by its `command` field. It is the
// shape produced by makeResleeveHandler ("<bin> hook --adapter codex"),
// independent of the absolute binary path so reinstalls from a moved
// binary still recognize (and replace) prior resleeve entries.
var resleeveHookCommandMarker = fmt.Sprintf("hook --adapter %s", Name)

// makeResleeveHandler builds the inner command object resleeve installs.
// We identify our own entries by the command string (it contains
// resleeveHookCommandMarker); no out-of-schema marker field is injected.
// When memoryOnly is true the command carries `--memory-only` so the hook
// skips event persistence.
func makeResleeveHandler(binPath string, memoryOnly bool) map[string]any {
	cmd := fmt.Sprintf("%s hook --adapter %s", binPath, Name)
	if memoryOnly {
		cmd += " --memory-only"
	}
	return map[string]any{
		"type":    "command",
		"command": cmd,
	}
}

// isResleeveHandler reports whether a leaf command handler is one of ours,
// recognized by its `command` string containing resleeveHookCommandMarker.
func isResleeveHandler(h any) bool {
	hm, _ := h.(map[string]any)
	if hm == nil {
		return false
	}
	cmd, _ := hm["command"].(string)
	return strings.Contains(cmd, resleeveHookCommandMarker)
}

// stripResleeveFromEvent walks an event's group array and returns a new
// slice with all resleeve inner handlers removed (identified by command
// string via isResleeveHandler). Groups whose
// inner hooks become empty are dropped; groups retaining non-resleeve
// hooks are preserved with only the resleeve entries filtered out.
func stripResleeveFromEvent(handlers []any) []any {
	out := make([]any, 0, len(handlers))
	for _, h := range handlers {
		hm, ok := h.(map[string]any)
		if !ok {
			out = append(out, h)
			continue
		}
		if rawInner, hasInner := hm["hooks"]; hasInner {
			innerArr, _ := rawInner.([]any)
			keptInner := make([]any, 0, len(innerArr))
			for _, ih := range innerArr {
				if !isResleeveHandler(ih) {
					keptInner = append(keptInner, ih)
				}
			}
			if len(keptInner) == 0 {
				continue
			}
			newGroup := make(map[string]any, len(hm))
			for k, v := range hm {
				newGroup[k] = v
			}
			newGroup["hooks"] = keptInner
			out = append(out, newGroup)
			continue
		}
		// Flat leaf (defensive — Codex uses wrapped groups).
		if isResleeveHandler(h) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// resolveBinPath returns the absolute, symlink-resolved path of the
// resleeve binary. Prefer the operator-supplied override; else derive
// from os.Executable.
func resolveBinPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve resleeve binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// hooksPath returns $CODEX_HOME/hooks.json (CODEX_HOME honored, default
// ~/.codex).
func hooksPath() string {
	return filepath.Join(codexHome(), "hooks.json")
}

func readHooksFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var file map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if file == nil {
		file = map[string]any{}
	}
	return file, nil
}

func writeHooksFile(path string, file map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	body = append(body, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
