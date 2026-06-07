package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// mcpServerName is the key under which resleeve registers itself in
// Antigravity's MCP config.
const mcpServerName = "resleeve"

// Antigravity (the gemini-flavored CLI) reads MCP servers from the shared
// gemini config at ~/.gemini/config/mcp_config.json (verified against the
// real install on this host: that file carries a top-level "mcpServers"
// object with "notebooks"/"visualization" entries, each
// {command, args}). Note: this is ~/.gemini/config/mcp_config.json, NOT
// ~/.gemini/antigravity-cli/mcp_config.json (the latter does not exist;
// antigravity-cli/mcp/ holds per-tool descriptors, not the server map).
//
// resleeve registers:
//
//	"mcpServers": {
//	  "resleeve": { "command": "<bin>", "args": ["mcp"] }
//	}
//
// We own ONLY the "resleeve" key. All other servers and keys are preserved.

// InstallMCP registers the resleeve MCP server in the gemini MCP config.
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

// UninstallMCP removes the resleeve MCP server, preserving user servers and
// every other key.
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

// mcpConfigPath returns ~/.gemini/config/mcp_config.json.
func mcpConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("homedir: %w", err)
	}
	return filepath.Join(home, ".gemini", "config", "mcp_config.json"), nil
}

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
