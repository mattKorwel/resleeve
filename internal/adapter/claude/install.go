package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// hookEventsToInstall is the set of Claude Code hook events resleeve
// wants to capture. UserPromptSubmit is present in Claude Code 2.1+;
// older versions silently ignore unknown event names so installing
// the full set is forward-safe.
var hookEventsToInstall = []string{
	"SessionStart",
	"PostToolUse",
	"Stop",
	"UserPromptSubmit",
}

// matcherAll is the matcher value installed for resleeve's hook groups.
// Empty string means "no filter" — fire on every event regardless of
// tool name (for tool-scoped events) or unconditionally (for events
// without a tool concept like SessionStart).
const matcherAll = ""

// Hook entries in ~/.claude/settings.json use a matcher-wrapped shape:
//
//   "PostToolUse": [
//     {
//       "matcher": "",
//       "hooks": [
//         { "type": "command", "command": "...", "_resleeve": true }
//       ]
//     }
//   ]
//
// Older versions of Claude Code accepted a flat shape (command objects
// directly inside the event array). Current CC validates against the
// wrapped shape and warns on the flat shape; we emit wrapped and
// migrate any pre-existing flat-format resleeve entries on install.
// User-authored flat entries are NOT migrated (we don't touch their
// data); users who want to silence CC's warnings on their own entries
// must migrate them themselves.

// InstallBridge writes resleeve hook entries to ~/.claude/settings.json.
// Idempotent: re-installation detects existing _resleeve-tagged entries
// (in either flat or wrapped format) and replaces them with a fresh
// wrapped entry; unrelated user-authored hooks are preserved untouched.
func (a *Adapter) InstallBridge(ctx context.Context, opts adapter.InstallOpts) error {
	binPath, err := resolveBinPath(opts.ResleeveBinPath)
	if err != nil {
		return err
	}

	path, err := settingsPath()
	if err != nil {
		return err
	}

	settings, err := readSettings(path)
	if err != nil {
		return err
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}

	handler := makeResleeveHandler(binPath, opts.MemoryOnly)

	// Memory-only mode installs ONLY SessionStart — capture for tool calls
	// / user prompts / session end is skipped entirely (no hooks fire for
	// those, so no events reach the daemon). SessionStart is still wired
	// so the additionalContext injection on session start keeps working;
	// the `--memory-only` flag baked into the command string tells the
	// hook command to skip its AppendEvents call too.
	eventsToInstall := hookEventsToInstall
	if opts.MemoryOnly {
		eventsToInstall = []string{"SessionStart"}
	}

	// In memory-only mode we ALSO need to strip any prior resleeve hooks
	// from the events we're NOT re-installing — otherwise a switch from
	// full → memory-only mode would leave stale PostToolUse/Stop/etc.
	// _resleeve hooks behind, still POSTing events to the daemon.
	if opts.MemoryOnly {
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
		// Strip prior resleeve entries (flat or wrapped) before
		// appending our own wrapped group. This is what makes
		// re-install idempotent AND migrates flat-format installs
		// from older resleeve versions.
		stripped := stripResleeveFromEvent(existing)
		ourGroup := map[string]any{
			"matcher": matcherAll,
			"hooks":   []any{handler},
		}
		hooks[eventName] = append(stripped, ourGroup)
	}

	if opts.DryRun {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(settings)
	}

	return writeSettings(path, settings)
}

// UninstallBridge removes resleeve hook entries from settings.json,
// preserving user-authored hooks. Handles both flat and wrapped formats
// so an uninstall after an old-format install still cleans up correctly.
func (a *Adapter) UninstallBridge(ctx context.Context) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	settings, err := readSettings(path)
	if err != nil {
		return err
	}
	hooks, ok := settings["hooks"].(map[string]any)
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
		delete(settings, "hooks")
	}
	return writeSettings(path, settings)
}

// makeResleeveHandler builds the inner command object resleeve installs.
// Same shape under both flat and wrapped formats — only the surrounding
// container changes. When memoryOnly is true the command picks up the
// `--memory-only` flag so the hook skips persisting events to the daemon
// (only the SessionStart additionalContext injection runs).
func makeResleeveHandler(binPath string, memoryOnly bool) map[string]any {
	// Claude Code executes hook commands through bash even on Windows,
	// where backslashes are escape characters that silently corrupt the
	// binary path (C:\Users\…\resleeve.exe -> C:Users…resleeve.exe ->
	// "command not found"). Emit forward slashes: bash leaves them intact
	// and the Windows executable accepts them just fine. No-op on unix.
	cmd := fmt.Sprintf("%s hook --adapter %s", filepath.ToSlash(binPath), Name)
	if memoryOnly {
		cmd += " --memory-only"
	}
	return map[string]any{
		"type":      "command",
		"command":   cmd,
		"_resleeve": true,
	}
}

// isResleeveHandler reports whether a handler object (the leaf
// "command" entry — under either format) is one of ours.
func isResleeveHandler(h any) bool {
	hm, _ := h.(map[string]any)
	if hm == nil {
		return false
	}
	v, _ := hm["_resleeve"].(bool)
	return v
}

// stripResleeveFromEvent walks an event's handler array (which may be a
// mix of flat command entries and matcher-wrapped groups) and returns a
// new slice with all _resleeve-tagged entries removed. Wrapped groups
// whose inner hooks become empty are dropped entirely; wrapped groups
// retaining non-resleeve hooks are preserved with only their inner
// _resleeve entries filtered out.
func stripResleeveFromEvent(handlers []any) []any {
	out := make([]any, 0, len(handlers))
	for _, h := range handlers {
		hm, ok := h.(map[string]any)
		if !ok {
			out = append(out, h)
			continue
		}
		// Wrapped format? An entry is wrapped if it carries a `hooks`
		// array (the matcher is optional in some schemas).
		if rawInner, hasInner := hm["hooks"]; hasInner {
			innerArr, _ := rawInner.([]any)
			keptInner := make([]any, 0, len(innerArr))
			for _, ih := range innerArr {
				if !isResleeveHandler(ih) {
					keptInner = append(keptInner, ih)
				}
			}
			if len(keptInner) == 0 {
				// Whole group was ours; drop it.
				continue
			}
			// Preserve all of the wrapper's other keys (matcher, etc.)
			// and substitute the filtered hooks array.
			newGroup := make(map[string]any, len(hm))
			for k, v := range hm {
				newGroup[k] = v
			}
			newGroup["hooks"] = keptInner
			out = append(out, newGroup)
			continue
		}
		// Flat format.
		if isResleeveHandler(h) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// resolveBinPath returns the absolute, symlink-resolved path of the
// resleeve binary. Prefer the operator-supplied opts value; else
// derive from os.Executable.
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

// settingsPath returns the path to Claude Code's user settings.json.
func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("homedir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func readSettings(path string) (map[string]any, error) {
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
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	return settings, nil
}

func writeSettings(path string, settings map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	body, err := json.MarshalIndent(settings, "", "  ")
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
