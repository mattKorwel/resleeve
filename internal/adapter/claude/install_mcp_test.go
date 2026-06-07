package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
	"github.com/mattkorwel/resleeve/internal/testutil"
)

// withTempHomeMCP points HOME at a fresh tmpdir and optionally seeds a
// ~/.claude.json with the given content. Returns the home dir.
func withTempHomeMCP(t *testing.T, seed string) string {
	t.Helper()
	dir := t.TempDir()
	if seed != "" {
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(seed), 0o600); err != nil {
			t.Fatalf("seed .claude.json: %v", err)
		}
	}
	testutil.SetHomeDir(t, dir)
	return dir
}

func readClaudeJSON(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse .claude.json: %v", err)
	}
	return m
}

func resleeveServer(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong type: %v", cfg["mcpServers"])
	}
	srv, ok := servers["resleeve"].(map[string]any)
	if !ok {
		t.Fatalf("resleeve server missing: %v", servers)
	}
	return srv
}

func TestInstallMCP_WritesEntry(t *testing.T) {
	home := withTempHomeMCP(t, "")
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/usr/local/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	srv := resleeveServer(t, readClaudeJSON(t, home))
	if srv["command"] != "/usr/local/bin/resleeve" {
		t.Errorf("command = %v, want /usr/local/bin/resleeve", srv["command"])
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
	if err := a.InstallMCP(context.Background(), opts); err != nil {
		t.Fatalf("InstallMCP #1: %v", err)
	}
	if err := a.InstallMCP(context.Background(), opts); err != nil {
		t.Fatalf("InstallMCP #2: %v", err)
	}
	servers := readClaudeJSON(t, home)["mcpServers"].(map[string]any)
	if len(servers) != 1 {
		t.Fatalf("expected exactly 1 server after re-install, got %d: %v", len(servers), servers)
	}
}

func TestInstallMCP_PreservesUserKeysAndServers(t *testing.T) {
	seed := `{"numStartups": 3, "mcpServers": {"other": {"command": "x", "args": ["y"]}}}`
	home := withTempHomeMCP(t, seed)
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	cfg := readClaudeJSON(t, home)
	if cfg["numStartups"] != float64(3) {
		t.Errorf("numStartups clobbered: %v", cfg["numStartups"])
	}
	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Errorf("user server 'other' dropped: %v", servers)
	}
	if _, ok := servers["resleeve"]; !ok {
		t.Errorf("resleeve server missing: %v", servers)
	}
}

func TestUninstallMCP_RemovesOnlyOurs(t *testing.T) {
	seed := `{"numStartups": 3, "mcpServers": {"other": {"command": "x"}, "resleeve": {"command": "z", "args": ["mcp"]}}}`
	home := withTempHomeMCP(t, seed)
	a := New()
	if err := a.UninstallMCP(context.Background()); err != nil {
		t.Fatalf("UninstallMCP: %v", err)
	}
	cfg := readClaudeJSON(t, home)
	if cfg["numStartups"] != float64(3) {
		t.Errorf("numStartups clobbered: %v", cfg["numStartups"])
	}
	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["resleeve"]; ok {
		t.Errorf("resleeve server not removed: %v", servers)
	}
	if _, ok := servers["other"]; !ok {
		t.Errorf("user server 'other' dropped: %v", servers)
	}
}

func TestUninstallMCP_DropsEmptyMCPServers(t *testing.T) {
	seed := `{"numStartups": 3, "mcpServers": {"resleeve": {"command": "z"}}}`
	home := withTempHomeMCP(t, seed)
	a := New()
	if err := a.UninstallMCP(context.Background()); err != nil {
		t.Fatalf("UninstallMCP: %v", err)
	}
	cfg := readClaudeJSON(t, home)
	if _, ok := cfg["mcpServers"]; ok {
		t.Errorf("empty mcpServers should be dropped: %v", cfg)
	}
}

func TestInstallMCP_DryRunWritesNothing(t *testing.T) {
	home := withTempHomeMCP(t, "")
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve", DryRun: true}); err != nil {
		t.Fatalf("InstallMCP dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf(".claude.json should not exist after dry-run, stat err=%v", err)
	}
}
