package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// withTempCodexHomeTOML points CODEX_HOME at a fresh tmpdir and optionally
// seeds config.toml. Returns the codex home dir.
func withTempCodexHomeTOML(t *testing.T, seed string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	if seed != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(seed), 0o600); err != nil {
			t.Fatalf("seed config.toml: %v", err)
		}
	}
	t.Setenv("CODEX_HOME", dir)
	return dir
}

func readConfigTOML(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	return string(data)
}

func TestInstallMCP_WritesBlock(t *testing.T) {
	home := withTempCodexHomeTOML(t, "")
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	got := readConfigTOML(t, home)
	for _, want := range []string{
		"[mcp_servers.resleeve]",
		`command = "/bin/resleeve"`,
		`args = ["mcp"]`,
		mcpBlockBegin,
		mcpBlockEnd,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.toml missing %q\n---\n%s", want, got)
		}
	}
}

func TestInstallMCP_Idempotent(t *testing.T) {
	home := withTempCodexHomeTOML(t, "")
	a := New()
	opts := adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}
	for i := 0; i < 2; i++ {
		if err := a.InstallMCP(context.Background(), opts); err != nil {
			t.Fatalf("InstallMCP #%d: %v", i, err)
		}
	}
	got := readConfigTOML(t, home)
	if n := strings.Count(got, "[mcp_servers.resleeve]"); n != 1 {
		t.Fatalf("want exactly 1 resleeve table, got %d\n---\n%s", n, got)
	}
	if n := strings.Count(got, mcpBlockBegin); n != 1 {
		t.Fatalf("want exactly 1 managed block, got %d", n)
	}
}

func TestInstallMCP_PreservesOtherTables(t *testing.T) {
	seed := "model = \"gpt-5\"\n\n[mcp_servers.node_repl]\ncommand = \"/x/node_repl\"\nargs = []\n"
	home := withTempCodexHomeTOML(t, seed)
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	got := readConfigTOML(t, home)
	if !strings.Contains(got, "[mcp_servers.node_repl]") {
		t.Errorf("user table node_repl dropped\n---\n%s", got)
	}
	if !strings.Contains(got, `model = "gpt-5"`) {
		t.Errorf("top-level key dropped\n---\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.resleeve]") {
		t.Errorf("resleeve table missing\n---\n%s", got)
	}
}

func TestUninstallMCP_RemovesOnlyOurs(t *testing.T) {
	seed := "model = \"gpt-5\"\n\n[mcp_servers.node_repl]\ncommand = \"/x/node_repl\"\n"
	home := withTempCodexHomeTOML(t, seed)
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	if err := a.UninstallMCP(context.Background()); err != nil {
		t.Fatalf("UninstallMCP: %v", err)
	}
	got := readConfigTOML(t, home)
	if strings.Contains(got, "[mcp_servers.resleeve]") {
		t.Errorf("resleeve table not removed\n---\n%s", got)
	}
	if strings.Contains(got, mcpBlockBegin) {
		t.Errorf("managed block sentinel left behind\n---\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.node_repl]") {
		t.Errorf("user table node_repl dropped\n---\n%s", got)
	}
	if !strings.Contains(got, `model = "gpt-5"`) {
		t.Errorf("top-level key dropped\n---\n%s", got)
	}
}

func TestUninstallMCP_RemovesFileWhenWeWereSoleContent(t *testing.T) {
	home := withTempCodexHomeTOML(t, "")
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve"}); err != nil {
		t.Fatalf("InstallMCP: %v", err)
	}
	if err := a.UninstallMCP(context.Background()); err != nil {
		t.Fatalf("UninstallMCP: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Errorf("config.toml should be removed when resleeve was sole content, stat=%v", err)
	}
}

func TestInstallMCP_DryRunWritesNothing(t *testing.T) {
	home := withTempCodexHomeTOML(t, "")
	a := New()
	if err := a.InstallMCP(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/bin/resleeve", DryRun: true}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Errorf("config.toml should not exist after dry-run: %v", err)
	}
}
