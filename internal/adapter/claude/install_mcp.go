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

// mcpServerName is the key under which resleeve registers itself in every
// CLI's MCP config. It is the stable identifier used for idempotent
// re-install and for uninstall (remove only our entry, preserve user
// servers).
const mcpServerName = "resleeve"

// Claude Code stores user-scoped MCP servers in ~/.claude.json under a
// top-level "mcpServers" object (verified against the real install on this
// host: ~/.claude.json carries the mcpServers key; ~/.claude/settings.json
// holds hooks/permissions but NOT mcpServers). The entry shape is:
//
//	"mcpServers": {
//	  "resleeve": { "command": "<bin>", "args": ["mcp"] }
//	}
//
// We own ONLY the "resleeve" key. All other top-level keys and other MCP
// servers are read and rewritten verbatim.

// InstallMCP registers the resleeve MCP server in ~/.claude.json.
// Idempotent: re-install overwrites our own "resleeve" entry and leaves
// every other key untouched.
func (a *Adapter) InstallMCP(ctx context.Context, opts adapter.InstallOpts) error {
	binPath, err := resolveBinPath(opts.ResleeveBinPath)
	if err != nil {
		return err
	}

	path, err := mcpConfigPath()
	if err != nil {
		return err
	}

	cfg, err := readJSONObject(path)
	if err != nil {
		return err
	}

	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		cfg["mcpServers"] = servers
	}
	servers[mcpServerName] = map[string]any{
		"command": binPath,
		"args":    []any{"mcp"},
	}

	if opts.DryRun {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg)
	}
	return writeJSONObject(path, cfg)
}

// UninstallMCP removes the resleeve MCP server from ~/.claude.json,
// preserving user-authored servers and every other key.
func (a *Adapter) UninstallMCP(ctx context.Context) error {
	path, err := mcpConfigPath()
	if err != nil {
		return err
	}
	cfg, err := readJSONObject(path)
	if err != nil {
		return err
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return nil
	}
	delete(servers, mcpServerName)
	if len(servers) == 0 {
		delete(cfg, "mcpServers")
	}
	return writeJSONObject(path, cfg)
}

// mcpConfigPath returns ~/.claude.json (Claude Code's user config that
// carries mcpServers).
func mcpConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("homedir: %w", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// readJSONObject reads a JSON object file, returning an empty map when the
// file is missing or empty. Shared by InstallMCP / UninstallMCP.
func readJSONObject(path string) (map[string]any, error) {
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
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}

// writeJSONObject writes a JSON object atomically with 2-space indent,
// mirroring writeSettings.
func writeJSONObject(path string, obj map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	body, err := json.MarshalIndent(obj, "", "  ")
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
