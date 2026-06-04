package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateSealKey_FreshCreatesFileWith0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "seal.key")

	key, err := LoadOrCreateSealKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateSealKey: %v", err)
	}
	if len(key) != sealKeyLen {
		t.Errorf("returned key length: got %d, want %d", len(key), sealKeyLen)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != int64(sealKeyLen) {
		t.Errorf("file size: got %d, want %d", info.Size(), sealKeyLen)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("file mode: got %o, want 0600", mode)
		}
	}
}

func TestLoadOrCreateSealKey_SecondCallReturnsSameKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seal.key")

	k1, err := LoadOrCreateSealKey(path)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	k2, err := LoadOrCreateSealKey(path)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("second LoadOrCreateSealKey returned a different key")
	}
}

func TestLoadOrCreateSealKey_WrongLengthErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seal.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := LoadOrCreateSealKey(path); err == nil {
		t.Fatal("expected error on wrong-length key file")
	}
}

func TestLoadOrCreateSealKey_EmptyPathErrors(t *testing.T) {
	if _, err := LoadOrCreateSealKey(""); err == nil {
		t.Fatal("expected error on empty path")
	}
}
