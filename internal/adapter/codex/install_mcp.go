package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// Codex reads MCP servers from $CODEX_HOME/config.toml under
// [mcp_servers.<name>] tables (verified against the real install on this
// host: ~/.codex/config.toml carries a [mcp_servers.node_repl] table with
// `command` / `args` keys). resleeve registers:
//
//	[mcp_servers.resleeve]
//	command = "<bin>"
//	args = ["mcp"]
//
// config.toml is USER-OWNED (unlike hooks.json which resleeve owns
// wholesale) and there is no TOML library in the dependency set, so we do
// NOT round-trip the whole file through a parser (that would reformat /
// drop comments and risk clobbering the user's config). Instead we manage
// our own marker-delimited block, identified by sentinel comment lines, and
// splice it in/out textually. Everything outside the block — including the
// user's other [mcp_servers.*] tables — is preserved byte-for-byte.

const (
	mcpServerName     = "resleeve"
	mcpBlockBegin     = "# >>> resleeve mcp (managed) >>>"
	mcpBlockEnd       = "# <<< resleeve mcp (managed) <<<"
	mcpBlockTableName = "[mcp_servers." + mcpServerName + "]"
)

// InstallMCP registers the resleeve MCP server in $CODEX_HOME/config.toml.
// Idempotent: re-install replaces the existing managed block; the rest of
// the file (other [mcp_servers.*] tables, comments, formatting) is
// preserved unchanged.
func (a *Adapter) InstallMCP(ctx context.Context, opts adapter.InstallOpts) error {
	binPath, err := resolveBinPath(opts.ResleeveBinPath)
	if err != nil {
		return err
	}

	path := mcpConfigPath()
	existing, err := readTextFile(path)
	if err != nil {
		return err
	}

	block := renderMCPBlock(binPath)
	updated := spliceMCPBlock(existing, block)

	if opts.DryRun {
		fmt.Print(updated)
		return nil
	}
	return writeTextFile(path, updated)
}

// UninstallMCP removes the resleeve managed block from config.toml,
// preserving the rest of the file. If the file would become empty it is
// removed (matching UninstallBridge's "owned empty file shouldn't linger"
// behavior — but only when WE were the sole content).
func (a *Adapter) UninstallMCP(ctx context.Context) error {
	path := mcpConfigPath()
	existing, err := readTextFile(path)
	if err != nil {
		return err
	}
	if existing == "" {
		return nil
	}
	updated := spliceMCPBlock(existing, "")
	if strings.TrimSpace(updated) == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	return writeTextFile(path, updated)
}

// renderMCPBlock builds the managed block text (including the sentinel
// comment lines and a trailing newline).
func renderMCPBlock(binPath string) string {
	var b strings.Builder
	b.WriteString(mcpBlockBegin)
	b.WriteByte('\n')
	b.WriteString(mcpBlockTableName)
	b.WriteByte('\n')
	fmt.Fprintf(&b, "command = %s\n", tomlQuote(binPath))
	b.WriteString("args = [\"mcp\"]\n")
	b.WriteString(mcpBlockEnd)
	b.WriteByte('\n')
	return b.String()
}

// spliceMCPBlock returns existing with any prior managed block removed and,
// when block is non-empty, the new block appended. Lines between (and
// including) the begin/end sentinels are dropped before splicing so
// re-install is idempotent.
func spliceMCPBlock(existing, block string) string {
	stripped := stripMCPBlock(existing)
	if block == "" {
		return stripped
	}
	var b strings.Builder
	b.WriteString(stripped)
	if stripped != "" && !strings.HasSuffix(stripped, "\n") {
		b.WriteByte('\n')
	}
	if stripped != "" {
		b.WriteByte('\n')
	}
	b.WriteString(block)
	return b.String()
}

// stripMCPBlock removes the managed block (begin..end inclusive) from text,
// tolerating leading/trailing whitespace on the sentinel lines. Returns the
// text with the block (and one adjacent blank line, if present) removed.
func stripMCPBlock(text string) string {
	if !strings.Contains(text, mcpBlockBegin) {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == mcpBlockBegin {
			inBlock = true
			// Drop a single trailing blank line we may have inserted
			// before the block on a prior install.
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			continue
		}
		if inBlock {
			if trimmed == mcpBlockEnd {
				inBlock = false
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// tomlQuote renders s as a TOML basic string with the minimal escaping
// (backslash and double-quote). Binary paths don't contain control chars in
// practice; this keeps us off a TOML dependency.
func tomlQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// mcpConfigPath returns $CODEX_HOME/config.toml (CODEX_HOME honored,
// default ~/.codex).
func mcpConfigPath() string {
	return filepath.Join(codexHome(), "config.toml")
}

func readTextFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

func writeTextFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
