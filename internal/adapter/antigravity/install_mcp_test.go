package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/testutil"
)

// withTempHomeMCP points HOME at a fresh tmpdir and optionally seeds
// ~/.gemini/config/mcp_config.json with the given content. Returns home.
func withTempHomeMCP(t *testing.T, seed string) string {
	t.Helper()
	dir := t.TempDir()
	if seed != "" {
		cfgDir := filepath.Join(dir, ".gemini", "config")
		if err := os.MkdirAll(cfgDir, 0o700); err != nil {
			t.Fatalf("mkdir gemini config: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "mcp_config.json"), []byte(seed), 0o600); err != nil {
			t.Fatalf("seed mcp_config.json: %v", err)
		}
	}
	testutil.SetHomeDir(t, dir)
	return dir
}

func readMCPConfig(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".gemini", "config", "mcp_config.json"))
	if err != nil {
		t.Fatalf("read mcp_config.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse mcp_config.json: %v", err)
	}
	return m
}

func servers(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	s, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing: %v", cfg)
	}
	return s
}

func TestInstallMCP_WritesEntry(t *testing.T) {
	home := withTempHomeMCP(t, "")
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	srv, ok := servers(t, readMCPConfig(t, home))["resleeve"].(map[string]any)
	if !ok {
		t.Fatalf("resleeve server missing")
	}
	if srv["command"] != "/bin/resleeve" {
		t.Errorf("command = %v", srv["command"])
	}
	args, _ := srv["args"].([]any)
	if len(args) != 1 || args[0] != "mcp" {
		t.Errorf("args = %v, want [mcp]", args)
	}
}

func TestInstallMCP_Idempotent(t *testing.T) {
	home := withTempHomeMCP(t, "")
	a := New()
	opts := adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}
	for i := 0; i < 2; i++ {
		if err := a.InstallMCP(context.Background(), opts); err != nil {
			t.Fatalf("InstallMCP #%d: %v", i, err)
		}
	}
	if n := len(servers(t, readMCPConfig(t, home))); n != 1 {
		t.Fatalf("want 1 server, got %d", n)
	}
}

func TestInstallMCP_PreservesUserServers(t *testing.T) {
	seed := `{"mcpServers": {"notebooks": {"command": "node", "args": ["x"]}}}`
	home := withTempHomeMCP(t, seed)
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	s := servers(t, readMCPConfig(t, home))
	if _, ok := s["notebooks"]; !ok {
		t.Errorf("user server 'notebooks' dropped: %v", s)
	}
	if _, ok := s["resleeve"]; !ok {
		t.Errorf("resleeve server missing: %v", s)
	}
}

func TestUninstallMCP_RemovesOnlyOurs(t *testing.T) {
	seed := `{"mcpServers": {"notebooks": {"command": "node"}, "resleeve": {"command": "z"}}}`
	home := withTempHomeMCP(t, seed)
	a := New()
	if err := a.UninstallMCP(context.Background()); err != nil {
		t.Fatalf("UninstallMCP: %v", err)
	}
	s := servers(t, readMCPConfig(t, home))
	if _, ok := s["resleeve"]; ok {
		t.Errorf("resleeve not removed: %v", s)
	}
	if _, ok := s["notebooks"]; !ok {
		t.Errorf("user server dropped: %v", s)
	}
}

func TestInstallMCP_DryRunWritesNothing(t *testing.T) {
	home := withTempHomeMCP(t, "")
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve", DryRun: true}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config", "mcp_config.json")); !os.IsNotExist(err) {
		t.Errorf("file should not exist after dry-run: %v", err)
	}
}
