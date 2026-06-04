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

// countResleeveHandlers walks the settings.json hooks tree counting
// _resleeve-tagged entries, looking inside both flat command entries
// and matcher-wrapped groups.
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
			if hm == nil {
				continue
			}
			// Wrapped group: walk its inner hooks.
			if inner, ok := hm["hooks"].([]any); ok {
				for _, ih := range inner {
					ihm, _ := ih.(map[string]any)
					if v, _ := ihm["_resleeve"].(bool); v {
						n++
					}
				}
				continue
			}
			// Flat command entry.
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

// TestInstallBridge_EmitsWrappedFormat verifies the new shape: each
// event's array contains matcher-wrapped groups, not flat command
// objects. This is what F12 fixes.
func TestInstallBridge_EmitsWrappedFormat(t *testing.T) {
	home := withTempHome(t, "")
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	m := readSettingsForTest(t, home)
	hooks, _ := m["hooks"].(map[string]any)
	for _, eventName := range hookEventsToInstall {
		arr, ok := hooks[eventName].([]any)
		if !ok || len(arr) == 0 {
			t.Errorf("%s: expected non-empty handler array", eventName)
			continue
		}
		for i, entry := range arr {
			hm, ok := entry.(map[string]any)
			if !ok {
				t.Errorf("%s[%d]: not an object", eventName, i)
				continue
			}
			if _, hasInner := hm["hooks"]; !hasInner {
				t.Errorf("%s[%d]: missing inner 'hooks' array (still flat format)", eventName, i)
			}
			if _, hasMatcher := hm["matcher"]; !hasMatcher {
				t.Errorf("%s[%d]: missing 'matcher' key", eventName, i)
			}
		}
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

// TestInstallBridge_PreservesUserAuthoredHooks: a pre-existing user
// hook (whether flat or wrapped) survives install untouched; resleeve's
// hook is appended as its own wrapped group.
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
		t.Fatalf("Stop should have 2 entries (user + resleeve), got %d", len(stop))
	}
	// First entry should be the user-authored flat handler (untouched).
	first, _ := stop[0].(map[string]any)
	if cmd, _ := first["command"].(string); cmd != "echo user-stop" {
		t.Errorf("user-authored Stop handler not preserved; first.command = %q", cmd)
	}
	// Second is ours: matcher-wrapped, with _resleeve handler inside.
	second, _ := stop[1].(map[string]any)
	if _, hasInner := second["hooks"]; !hasInner {
		t.Errorf("resleeve entry should be wrapped (have inner 'hooks'); got %v", second)
	}
	inner, _ := second["hooks"].([]any)
	if len(inner) != 1 {
		t.Fatalf("expected 1 inner hook, got %d", len(inner))
	}
	ih, _ := inner[0].(map[string]any)
	if v, _ := ih["_resleeve"].(bool); !v {
		t.Errorf("inner hook not marked _resleeve: %v", ih)
	}
}

// TestInstallBridge_MigratesFlatFormat: when a settings.json contains
// flat-format resleeve hooks from an older install, re-installing
// should remove the flat entries and replace them with wrapped groups.
// This is the F12 migration path users on existing installs land on.
func TestInstallBridge_MigratesFlatFormat(t *testing.T) {
	seed := `{
		"hooks": {
			"SessionStart": [
				{ "type": "command", "command": "/old/resleeve hook --adapter claude", "_resleeve": true }
			],
			"Stop": [
				{ "type": "command", "command": "echo user-stop" },
				{ "type": "command", "command": "/old/resleeve hook --adapter claude", "_resleeve": true }
			]
		}
	}`
	home := withTempHome(t, seed)
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/new/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	m := readSettingsForTest(t, home)
	hooks, _ := m["hooks"].(map[string]any)

	// SessionStart should have exactly one entry: our new wrapped group,
	// pointing at the new bin path (NOT the old).
	ss, _ := hooks["SessionStart"].([]any)
	if len(ss) != 1 {
		t.Fatalf("SessionStart: expected exactly 1 entry post-migration, got %d", len(ss))
	}
	ssEntry, _ := ss[0].(map[string]any)
	ssInner, _ := ssEntry["hooks"].([]any)
	if len(ssInner) != 1 {
		t.Fatalf("SessionStart inner hooks: expected 1, got %d", len(ssInner))
	}
	ssHook, _ := ssInner[0].(map[string]any)
	if cmd, _ := ssHook["command"].(string); !strings.Contains(cmd, "/new/resleeve") {
		t.Errorf("SessionStart command not updated to new bin: %q", cmd)
	}

	// Stop should have 2 entries: the user's flat handler (untouched)
	// and our new wrapped group. The old flat resleeve entry is gone.
	stop, _ := hooks["Stop"].([]any)
	if len(stop) != 2 {
		t.Fatalf("Stop: expected 2 entries (user flat + resleeve wrapped), got %d", len(stop))
	}
	// User entry first.
	userEntry, _ := stop[0].(map[string]any)
	if cmd, _ := userEntry["command"].(string); cmd != "echo user-stop" {
		t.Errorf("user Stop handler not preserved: %q", cmd)
	}
	// Resleeve entry second, wrapped.
	rsEntry, _ := stop[1].(map[string]any)
	if _, hasInner := rsEntry["hooks"]; !hasInner {
		t.Errorf("Stop resleeve entry should be wrapped, got %v", rsEntry)
	}

	// No remaining flat-format resleeve entries anywhere.
	for eventName, raw := range hooks {
		arr, _ := raw.([]any)
		for i, h := range arr {
			hm, _ := h.(map[string]any)
			if hm == nil {
				continue
			}
			// Flat resleeve = command-shaped + _resleeve:true.
			if _, isWrapped := hm["hooks"]; isWrapped {
				continue
			}
			if v, _ := hm["_resleeve"].(bool); v {
				t.Errorf("%s[%d]: flat-format resleeve entry survived migration: %v", eventName, i, hm)
			}
		}
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

// TestUninstallBridge_RemovesFlatFormat: even if a pre-existing
// settings.json has flat-format resleeve entries (from an older
// install) and we never wrapped them, uninstall still finds and
// removes them.
func TestUninstallBridge_RemovesFlatFormat(t *testing.T) {
	seed := `{
		"hooks": {
			"SessionStart": [
				{ "type": "command", "command": "/old/resleeve hook --adapter claude", "_resleeve": true }
			]
		}
	}`
	home := withTempHome(t, seed)
	a := New()
	if err := a.UninstallBridge(context.Background()); err != nil {
		t.Fatalf("UninstallBridge: %v", err)
	}
	m := readSettingsForTest(t, home)
	if n := countResleeveHandlers(t, m); n != 0 {
		t.Errorf("flat-format resleeve handler survived uninstall, count=%d", n)
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
