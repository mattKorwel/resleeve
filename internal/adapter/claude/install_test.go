package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// withTempHome runs fn with HOME pointing at a fresh tmpdir containing
// a ~/.claude/ directory and the given seed settings.json content
// (empty string = no settings.json). Restores HOME after fn returns.
func withTempHome(t *testing.T, seed string) string {
	t.Helper()
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if seed != "" {
		if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(seed), 0o600); err != nil {
			t.Fatalf("seed settings.json: %v", err)
		}
	}
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", dir)
	t.Cleanup(func() { _ = os.Setenv("HOME", oldHome) })
	return dir
}

func readSettingsForTest(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return m
}

func countResleeveHandlers(t *testing.T, m map[string]any) int {
	t.Helper()
	hooks, _ := m["hooks"].(map[string]any)
	if hooks == nil {
		return 0
	}
	n := 0
	for _, raw := range hooks {
		arr, _ := raw.([]any)
		for _, h := range arr {
			hm, _ := h.(map[string]any)
			if v, _ := hm["_resleeve"].(bool); v {
				n++
			}
		}
	}
	return n
}

func TestInstallBridge_NoSettingsFile(t *testing.T) {
	home := withTempHome(t, "")
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	m := readSettingsForTest(t, home)
	if n := countResleeveHandlers(t, m); n != len(hookEventsToInstall) {
		t.Errorf("got %d _resleeve handlers, want %d", n, len(hookEventsToInstall))
	}
}

func TestInstallBridge_PreservesThemeKey(t *testing.T) {
	home := withTempHome(t, `{"theme":"dark"}`)
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	m := readSettingsForTest(t, home)
	if theme, _ := m["theme"].(string); theme != "dark" {
		t.Errorf("theme not preserved: got %q, want %q", theme, "dark")
	}
	if n := countResleeveHandlers(t, m); n != len(hookEventsToInstall) {
		t.Errorf("got %d _resleeve handlers, want %d", n, len(hookEventsToInstall))
	}
}

func TestInstallBridge_PreservesUserAuthoredHooks(t *testing.T) {
	seed := `{
		"theme": "dark",
		"hooks": {
			"Stop": [
				{ "type": "command", "command": "echo user-stop" }
			]
		}
	}`
	home := withTempHome(t, seed)
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	m := readSettingsForTest(t, home)
	hooks, _ := m["hooks"].(map[string]any)
	stop, _ := hooks["Stop"].([]any)
	if len(stop) != 2 {
		t.Fatalf("Stop should have 2 handlers (user + resleeve), got %d", len(stop))
	}
	// First entry should be the user-authored handler (unchanged).
	first, _ := stop[0].(map[string]any)
	if cmd, _ := first["command"].(string); cmd != "echo user-stop" {
		t.Errorf("user-authored Stop handler not preserved; first.command = %q", cmd)
	}
	// Second is ours.
	second, _ := stop[1].(map[string]any)
	if v, _ := second["_resleeve"].(bool); !v {
		t.Errorf("resleeve handler not appended to Stop")
	}
}

func TestInstallBridge_Idempotent(t *testing.T) {
	home := withTempHome(t, `{"theme":"auto"}`)
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("first InstallBridge: %v", err)
	}
	first, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("second InstallBridge: %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if string(first) != string(second) {
		t.Errorf("re-install produced a different settings.json")
	}
}

func TestUninstallBridge_RemovesOursPreservesOthers(t *testing.T) {
	seed := `{
		"theme": "dark",
		"hooks": {
			"Stop": [
				{ "type": "command", "command": "echo user-stop" }
			]
		}
	}`
	home := withTempHome(t, seed)
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	if err := a.UninstallBridge(context.Background()); err != nil {
		t.Fatalf("UninstallBridge: %v", err)
	}
	m := readSettingsForTest(t, home)
	if n := countResleeveHandlers(t, m); n != 0 {
		t.Errorf("expected 0 _resleeve handlers after uninstall, got %d", n)
	}
	hooks, _ := m["hooks"].(map[string]any)
	stop, _ := hooks["Stop"].([]any)
	if len(stop) != 1 {
		t.Fatalf("user Stop should remain (1 handler), got %d", len(stop))
	}
	first, _ := stop[0].(map[string]any)
	if cmd, _ := first["command"].(string); cmd != "echo user-stop" {
		t.Errorf("user Stop handler not preserved: %q", cmd)
	}
	if theme, _ := m["theme"].(string); theme != "dark" {
		t.Errorf("theme not preserved after uninstall")
	}
}

func TestInstallBridge_BinPathInCommand(t *testing.T) {
	withTempHome(t, "")
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/opt/resleeve/bin/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"))
	if !strings.Contains(string(data), "/opt/resleeve/bin/resleeve hook --adapter claude") {
		t.Errorf("expected command to include the overridden bin path, got: %s", string(data))
	}
}
