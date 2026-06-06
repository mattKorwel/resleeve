package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

// withTempCodexHome points CODEX_HOME at a fresh tmpdir and seeds an
// optional hooks.json. Returns the codex home dir.
func withTempCodexHome(t *testing.T, seed string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	if seed != "" {
		if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(seed), 0o600); err != nil {
			t.Fatalf("seed hooks.json: %v", err)
		}
	}
	t.Setenv("CODEX_HOME", dir)
	return dir
}

func readHooksForTest(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	return m
}

// countResleeveHandlers walks the hooks tree counting inner command
// handlers recognized as resleeve's own by their command string. It also
// fails the test if any handler carries the old out-of-schema `_resleeve`
// marker field, which codex's schema validation could reject.
func countResleeveHandlers(t *testing.T, m map[string]any) int {
	t.Helper()
	hooks, _ := m["hooks"].(map[string]any)
	if hooks == nil {
		return 0
	}
	n := 0
	for _, raw := range hooks {
		arr, _ := raw.([]any)
		for _, g := range arr {
			gm, _ := g.(map[string]any)
			inner, _ := gm["hooks"].([]any)
			for _, ih := range inner {
				ihm, _ := ih.(map[string]any)
				if _, has := ihm["_resleeve"]; has {
					t.Errorf("handler carries out-of-schema _resleeve field: %v", ihm)
				}
				if cmd, _ := ihm["command"].(string); strings.Contains(cmd, "hook --adapter codex") {
					n++
				}
			}
		}
	}
	return n
}

func TestInstallBridge_NoHooksFile(t *testing.T) {
	home := withTempCodexHome(t, "")
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	m := readHooksForTest(t, home)
	if n := countResleeveHandlers(t, m); n != len(hookEventsToInstall) {
		t.Errorf("got %d _resleeve handlers, want %d", n, len(hookEventsToInstall))
	}
}

// TestInstallBridge_WrappedShape verifies each event holds matcher-group
// objects with an inner "hooks" array (the shape Codex parses).
func TestInstallBridge_WrappedShape(t *testing.T) {
	home := withTempCodexHome(t, "")
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	m := readHooksForTest(t, home)
	hooks, _ := m["hooks"].(map[string]any)
	for _, eventName := range hookEventsToInstall {
		arr, ok := hooks[eventName].([]any)
		if !ok || len(arr) == 0 {
			t.Errorf("%s: expected non-empty group array", eventName)
			continue
		}
		g, _ := arr[0].(map[string]any)
		inner, ok := g["hooks"].([]any)
		if !ok || len(inner) != 1 {
			t.Errorf("%s: expected inner hooks array of 1, got %v", eventName, g)
			continue
		}
		ih, _ := inner[0].(map[string]any)
		if ih["type"] != "command" {
			t.Errorf("%s: inner type = %v, want command", eventName, ih["type"])
		}
		if cmd, _ := ih["command"].(string); !strings.Contains(cmd, "hook --adapter codex") {
			t.Errorf("%s: command = %q", eventName, cmd)
		}
	}
}

func TestInstallBridge_PreservesUserHooks(t *testing.T) {
	seed := `{
		"hooks": {
			"Stop": [
				{ "hooks": [ { "type": "command", "command": "echo user-stop" } ] }
			]
		}
	}`
	home := withTempCodexHome(t, seed)
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	m := readHooksForTest(t, home)
	hooks, _ := m["hooks"].(map[string]any)
	stop, _ := hooks["Stop"].([]any)
	if len(stop) != 2 {
		t.Fatalf("Stop should have 2 groups (user + resleeve), got %d", len(stop))
	}
	first, _ := stop[0].(map[string]any)
	fi, _ := first["hooks"].([]any)
	fih, _ := fi[0].(map[string]any)
	if fih["command"] != "echo user-stop" {
		t.Errorf("user Stop hook not preserved: %v", fih)
	}
}

func TestInstallBridge_Idempotent(t *testing.T) {
	home := withTempCodexHome(t, "")
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("first InstallBridge: %v", err)
	}
	first, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("second InstallBridge: %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	if string(first) != string(second) {
		t.Errorf("re-install produced a different hooks.json:\n%s\n---\n%s", first, second)
	}
}

func TestInstallBridge_MemoryOnly(t *testing.T) {
	home := withTempCodexHome(t, "")
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve", MemoryOnly: true}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	m := readHooksForTest(t, home)
	hooks, _ := m["hooks"].(map[string]any)
	if _, ok := hooks["SessionStart"]; !ok {
		t.Error("memory-only should install SessionStart")
	}
	for _, e := range []string{"PostToolUse", "Stop", "UserPromptSubmit"} {
		if _, ok := hooks[e]; ok {
			t.Errorf("memory-only should not install %s", e)
		}
	}
	// command must carry --memory-only.
	ss, _ := hooks["SessionStart"].([]any)
	g, _ := ss[0].(map[string]any)
	inner, _ := g["hooks"].([]any)
	ih, _ := inner[0].(map[string]any)
	if cmd, _ := ih["command"].(string); !strings.Contains(cmd, "--memory-only") {
		t.Errorf("expected --memory-only in command, got %q", cmd)
	}
}

func TestUninstallBridge_RemovesOursPreservesOthers(t *testing.T) {
	seed := `{
		"hooks": {
			"Stop": [
				{ "hooks": [ { "type": "command", "command": "echo user-stop" } ] }
			]
		}
	}`
	home := withTempCodexHome(t, seed)
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	if err := a.UninstallBridge(context.Background()); err != nil {
		t.Fatalf("UninstallBridge: %v", err)
	}
	m := readHooksForTest(t, home)
	if n := countResleeveHandlers(t, m); n != 0 {
		t.Errorf("expected 0 _resleeve handlers after uninstall, got %d", n)
	}
	hooks, _ := m["hooks"].(map[string]any)
	stop, _ := hooks["Stop"].([]any)
	if len(stop) != 1 {
		t.Fatalf("user Stop should remain, got %d groups", len(stop))
	}
}

// TestUninstallBridge_RemovesFileWhenWeOwnIt: when resleeve created the
// whole hooks.json (no user hooks), uninstall removes the file entirely.
func TestUninstallBridge_RemovesFileWhenWeOwnIt(t *testing.T) {
	home := withTempCodexHome(t, "")
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve"}); err != nil {
		t.Fatalf("InstallBridge: %v", err)
	}
	if err := a.UninstallBridge(context.Background()); err != nil {
		t.Fatalf("UninstallBridge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "hooks.json")); !os.IsNotExist(err) {
		t.Errorf("expected hooks.json removed when fully owned, stat err = %v", err)
	}
}

func TestInstallBridge_DryRunWritesNothing(t *testing.T) {
	home := withTempCodexHome(t, "")
	a := New()
	if err := a.InstallBridge(context.Background(), adapter.InstallOpts{ResleeveBinPath: "/tmp/resleeve", DryRun: true}); err != nil {
		t.Fatalf("InstallBridge dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "hooks.json")); !os.IsNotExist(err) {
		t.Errorf("dry-run should not write hooks.json, stat err = %v", err)
	}
}
