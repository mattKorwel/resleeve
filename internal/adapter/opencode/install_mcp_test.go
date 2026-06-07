package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// withTempConfig points XDG_CONFIG_HOME at a fresh tmpdir and optionally
// seeds opencode/<name> with the given content. Returns the resolved
// opencode config dir.
func withTempConfig(t *testing.T, name, seed string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	ocDir := filepath.Join(dir, "opencode")
	if seed != "" {
		if err := os.MkdirAll(ocDir, 0o700); err != nil {
			t.Fatalf("mkdir opencode: %v", err)
		}
		if err := os.WriteFile(filepath.Join(ocDir, name), []byte(seed), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return ocDir
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse config (must be valid JSON): %v", err)
	}
	return m
}

func mcpServers(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	m, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("mcp object missing: %v", cfg)
	}
	return m
}

func TestInstallMCP_WritesEntry(t *testing.T) {
	ocDir := withTempConfig(t, "", "")
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	// No seed → defaults to opencode.json.
	cfg := readConfig(t, filepath.Join(ocDir, "opencode.json"))
	srv, ok := mcpServers(t, cfg)["resleeve"].(map[string]any)
	if !ok {
		t.Fatalf("resleeve server missing: %v", cfg)
	}
	if srv["type"] != "local" {
		t.Errorf("type = %v, want local", srv["type"])
	}
	if srv["enabled"] != true {
		t.Errorf("enabled = %v, want true", srv["enabled"])
	}
	cmd, _ := srv["command"].([]any)
	if len(cmd) != 2 || cmd[0] != "/bin/resleeve" || cmd[1] != "mcp" {
		t.Errorf("command = %v, want [/bin/resleeve mcp]", cmd)
	}
}

func TestInstallMCP_PrefersExistingJSONC(t *testing.T) {
	ocDir := withTempConfig(t, "opencode.jsonc", `{"$schema": "https://opencode.ai/config.json"}`)
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	// Should write back to the jsonc file, preserving $schema.
	cfg := readConfig(t, filepath.Join(ocDir, "opencode.jsonc"))
	if cfg["$schema"] != "https://opencode.ai/config.json" {
		t.Errorf("$schema dropped: %v", cfg)
	}
	if _, ok := mcpServers(t, cfg)["resleeve"]; !ok {
		t.Errorf("resleeve server missing: %v", cfg)
	}
	// The json variant should NOT have been created.
	if _, err := os.Stat(filepath.Join(ocDir, "opencode.json")); !os.IsNotExist(err) {
		t.Errorf("opencode.json should not exist, stat=%v", err)
	}
}

func TestInstallMCP_StripsLineComments(t *testing.T) {
	seed := "{\n  // user comment\n  \"$schema\": \"x\"\n}"
	ocDir := withTempConfig(t, "opencode.jsonc", seed)
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	cfg := readConfig(t, filepath.Join(ocDir, "opencode.jsonc"))
	if cfg["$schema"] != "x" {
		t.Errorf("$schema dropped: %v", cfg)
	}
	if _, ok := mcpServers(t, cfg)["resleeve"]; !ok {
		t.Errorf("resleeve server missing: %v", cfg)
	}
}

func TestInstallMCP_Idempotent(t *testing.T) {
	ocDir := withTempConfig(t, "", "")
	a := New()
	opts := adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}
	for i := 0; i < 2; i++ {
		if err := a.InstallMCP(context.Background(), opts); err != nil {
			t.Fatalf("InstallMCP #%d: %v", i, err)
		}
	}
	cfg := readConfig(t, filepath.Join(ocDir, "opencode.json"))
	if n := len(mcpServers(t, cfg)); n != 1 {
		t.Fatalf("want 1 server, got %d", n)
	}
}

func TestUninstallMCP_RemovesOnlyOurs(t *testing.T) {
	seed := `{"$schema": "x", "mcp": {"other": {"type": "local", "command": ["y"]}, "resleeve": {"type": "local", "command": ["z", "mcp"]}}}`
	ocDir := withTempConfig(t, "opencode.json", seed)
	a := New()
	if err := a.UninstallMCP(context.Background()); err != nil {
		t.Fatalf("UninstallMCP: %v", err)
	}
	cfg := readConfig(t, filepath.Join(ocDir, "opencode.json"))
	if cfg["$schema"] != "x" {
		t.Errorf("$schema dropped: %v", cfg)
	}
	m := mcpServers(t, cfg)
	if _, ok := m["resleeve"]; ok {
		t.Errorf("resleeve not removed: %v", m)
	}
	if _, ok := m["other"]; !ok {
		t.Errorf("user server dropped: %v", m)
	}
}

func TestInstallMCP_DryRunWritesNothing(t *testing.T) {
	ocDir := withTempConfig(t, "", "")
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve", DryRun: true}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ocDir, "opencode.json")); !os.IsNotExist(err) {
		t.Errorf("file should not exist after dry-run: %v", err)
	}
}
