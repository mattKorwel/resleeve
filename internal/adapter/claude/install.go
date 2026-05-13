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

// InstallBridge writes resleeve hook entries to ~/.claude/settings.json.
// Idempotent: re-installation detects existing _resleeve-tagged entries
// and updates them in place; unrelated user-authored hooks are preserved.
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

	command := fmt.Sprintf("%s hook --adapter %s", binPath, Name)

	for _, eventName := range hookEventsToInstall {
		existing, _ := hooks[eventName].([]any)
		newHandler := map[string]any{
			"type":      "command",
			"command":   command,
			"_resleeve": true,
		}
		merged := make([]any, 0, len(existing)+1)
		replaced := false
		for _, h := range existing {
			hm, ok := h.(map[string]any)
			if !ok {
				merged = append(merged, h)
				continue
			}
			if isResleeve, _ := hm["_resleeve"].(bool); isResleeve {
				merged = append(merged, newHandler)
				replaced = true
				continue
			}
			merged = append(merged, h)
		}
		if !replaced {
			merged = append(merged, newHandler)
		}
		hooks[eventName] = merged
	}

	if opts.DryRun {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(settings)
	}

	return writeSettings(path, settings)
}

// UninstallBridge removes resleeve hook entries from settings.json,
// preserving user-authored hooks.
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
		kept := make([]any, 0, len(handlers))
		for _, h := range handlers {
			hm, ok := h.(map[string]any)
			if !ok {
				kept = append(kept, h)
				continue
			}
			if isResleeve, _ := hm["_resleeve"].(bool); isResleeve {
				continue
			}
			kept = append(kept, h)
		}
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
