package opencode

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

// mcpServerName is the key under which resleeve registers itself in
// opencode's MCP config.
const mcpServerName = "resleeve"

// opencode reads its config from ~/.config/opencode/opencode.json[c]
// (honoring $XDG_CONFIG_HOME). MCP servers live under a top-level "mcp"
// object keyed by server name. The local-transport entry shape (verified
// against opencode's config schema; the real install on this host has an
// opencode.jsonc carrying only "$schema") is:
//
//	"mcp": {
//	  "resleeve": {
//	    "type": "local",
//	    "command": ["<bin>", "mcp"],
//	    "enabled": true
//	  }
//	}
//
// We own ONLY the "resleeve" key under "mcp"; all other keys and other MCP
// servers are preserved. The file is JSONC (JSON-with-comments). The real
// install is plain JSON; we parse with encoding/json after stripping
// whole-line `//` comments (the only comment form opencode's default config
// uses). If a config contains block comments / trailing commas we cannot
// safely round-trip, so InstallMCP returns an error rather than corrupt the
// file.

// InstallMCP registers the resleeve MCP server in opencode's config.
func (a *Adapter) InstallMCP(ctx context.Context, opts adapter.InstallOpts) error {
	binPath, err := resolveBinPath(opts.ResleeveBinPath)
	if err != nil {
		return err
	}

	path, err := mcpConfigPath()
	if err != nil {
		return err
	}

	cfg, err := readOpencodeConfig(path)
	if err != nil {
		return err
	}

	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
		cfg["mcp"] = mcp
	}
	mcp[mcpServerName] = map[string]any{
		"type":    "local",
		"command": []any{binPath, "mcp"},
		"enabled": true,
	}

	if opts.DryRun {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg)
	}
	return writeOpencodeConfig(path, cfg)
}

// UninstallMCP removes the resleeve MCP server from opencode's config,
// preserving user-authored servers and every other key.
func (a *Adapter) UninstallMCP(ctx context.Context) error {
	path, err := mcpConfigPath()
	if err != nil {
		return err
	}
	cfg, err := readOpencodeConfig(path)
	if err != nil {
		return err
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		return nil
	}
	delete(mcp, mcpServerName)
	if len(mcp) == 0 {
		delete(cfg, "mcp")
	}
	return writeOpencodeConfig(path, cfg)
}

// resolveBinPath returns the absolute, symlink-resolved path of the
// resleeve binary. Prefer the operator-supplied override; else derive from
// os.Executable.
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

// mcpConfigPath returns the opencode config file path. Prefers an existing
// opencode.jsonc, then opencode.json; defaults to opencode.json when
// neither exists. Honors $XDG_CONFIG_HOME, falling back to ~/.config.
func mcpConfigPath() (string, error) {
	dir, err := opencodeConfigDir()
	if err != nil {
		return "", err
	}
	jsonc := filepath.Join(dir, "opencode.jsonc")
	if _, err := os.Stat(jsonc); err == nil {
		return jsonc, nil
	}
	return filepath.Join(dir, "opencode.json"), nil
}

func opencodeConfigDir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("homedir: %w", err)
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

// readOpencodeConfig reads and parses the opencode config, returning an
// empty object when the file is missing/empty. Strips whole-line `//`
// comments before parsing (opencode's only default comment form).
func readOpencodeConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	cleaned := stripLineComments(string(data))
	var cfg map[string]any
	if err := json.Unmarshal([]byte(cleaned), &cfg); err != nil {
		return nil, fmt.Errorf("parse %s (JSONC with block comments / trailing commas is unsupported): %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

// stripLineComments removes lines whose first non-whitespace characters are
// `//`. It does NOT strip inline (mid-line) comments or block comments —
// the default opencode config carries none, and conservative handling
// avoids mangling `//` sequences inside string values.
func stripLineComments(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func writeOpencodeConfig(path string, cfg map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
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
