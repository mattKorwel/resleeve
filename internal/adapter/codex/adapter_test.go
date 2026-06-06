package codex

import (
	"context"
	"testing"

	"github.com/mattkorwel/resleeve/internal/adapter"
)

func TestName(t *testing.T) {
	if New().Name() != "codex" {
		t.Fatalf("Name() = %q, want codex", New().Name())
	}
}

// TestDetect_NotInstalled exercises the not-found path by pointing PATH at
// an empty dir. Detect must never error and must still report codex_home.
func TestDetect_NotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CODEX_HOME", "/custom/codex/home")
	d, err := New().Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Installed {
		t.Errorf("expected not installed")
	}
	if d.Quirks["codex_home"] != "/custom/codex/home" {
		t.Errorf("codex_home quirk: got %q", d.Quirks["codex_home"])
	}
}

func TestParseVersion(t *testing.T) {
	cases := map[string]string{
		"codex-cli 0.121.0\n": "0.121.0",
		"0.137.0":             "0.137.0",
		"codex-cli 1.2.3":     "1.2.3",
		"":                    "",
	}
	for in, want := range cases {
		if got := parseVersion(in); got != want {
			t.Errorf("parseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCodexHome_Override(t *testing.T) {
	t.Setenv("CODEX_HOME", "/xyz/.codex")
	if got := codexHome(); got != "/xyz/.codex" {
		t.Errorf("codexHome() = %q, want /xyz/.codex", got)
	}
}

func TestNewSessionUUIDv7_IsV7(t *testing.T) {
	id := NewSessionUUIDv7()
	// version nibble is the first char of the 3rd group.
	// 019e90a2-c115-7523-... → '7'
	if len(id) != 36 {
		t.Fatalf("uuid wrong length: %q", id)
	}
	if id[14] != '7' {
		t.Errorf("expected UUIDv7 (version nibble '7'), got %q (char %q)", id, string(id[14]))
	}
}

// compile-time guard: *Adapter satisfies adapter.Adapter.
var _ adapter.Adapter = (*Adapter)(nil)
