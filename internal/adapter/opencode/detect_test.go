package opencode

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mattkorwel/resleeve/internal/testutil"
)

// fakeOpencodeBin writes an executable named opencode (or opencode.exe on
// Windows) into dir and returns its path.
func fakeOpencodeBin(t *testing.T, dir string) string {
	t.Helper()
	name := "opencode"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestDetect_MissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	d, err := New().Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Installed {
		t.Errorf("expected Installed=false with empty PATH, got %+v", d)
	}
}

func TestDetect_SqliteEraPresent(t *testing.T) {
	binDir := t.TempDir()
	target := fakeOpencodeBin(t, binDir)
	t.Setenv("PATH", binDir)

	// Point the data dir at a temp home with a real opencode.db file.
	home := t.TempDir()
	testutil.SetHomeDir(t, home)
	t.Setenv("XDG_DATA_HOME", "") // force the ~/.local/share fallback
	dataDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "opencode.db")
	if err := os.WriteFile(dbPath, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := New().Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !d.Installed {
		t.Fatalf("expected Installed=true, got %+v", d)
	}
	if d.Path != target {
		t.Errorf("Path: got %q, want %q", d.Path, target)
	}
	if _, bad := d.Quirks["unsupported"]; bad {
		t.Errorf("expected supported (no unsupported quirk), got %+v", d.Quirks)
	}
	if d.Quirks["db_path"] != dbPath {
		t.Errorf("db_path quirk: got %q, want %q", d.Quirks["db_path"], dbPath)
	}
}

func TestDetect_PreSqliteUnsupported(t *testing.T) {
	binDir := t.TempDir()
	fakeOpencodeBin(t, binDir)
	t.Setenv("PATH", binDir)

	home := t.TempDir()
	testutil.SetHomeDir(t, home)
	t.Setenv("XDG_DATA_HOME", "")
	// No opencode.db created → pre-sqlite / unsupported.

	d, err := New().Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !d.Installed {
		t.Fatalf("expected Installed=true (binary present), got %+v", d)
	}
	if d.Quirks["unsupported"] != "pre-sqlite" {
		t.Errorf("expected unsupported=pre-sqlite quirk, got %+v", d.Quirks)
	}
}

func TestDetect_OpencodeDBEnvOverride(t *testing.T) {
	binDir := t.TempDir()
	fakeOpencodeBin(t, binDir)
	t.Setenv("PATH", binDir)

	home := t.TempDir()
	testutil.SetHomeDir(t, home)
	t.Setenv("XDG_DATA_HOME", "")

	// Absolute $OPENCODE_DB override pointing at an existing file.
	customDB := filepath.Join(t.TempDir(), "custom.db")
	if err := os.WriteFile(customDB, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_DB", customDB)

	d, err := New().Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Quirks["db_path"] != customDB {
		t.Errorf("db_path quirk: got %q, want %q (env override)", d.Quirks["db_path"], customDB)
	}
	if _, bad := d.Quirks["unsupported"]; bad {
		t.Errorf("expected supported with env-override db present, got %+v", d.Quirks)
	}
}
