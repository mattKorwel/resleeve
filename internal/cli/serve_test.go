package cli

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMasterKey_HexFile(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "mk.hex")
	if err := os.WriteFile(f, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadMasterKey(f)
	if err != nil {
		t.Fatalf("loadMasterKey: %v", err)
	}
	if string(got) != string(key) {
		t.Fatalf("hex decode mismatch")
	}
}

func TestLoadMasterKey_Base64File(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(255 - i)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "mk.b64")
	if err := os.WriteFile(f, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadMasterKey(f)
	if err != nil {
		t.Fatalf("loadMasterKey: %v", err)
	}
	if string(got) != string(key) {
		t.Fatalf("base64 decode mismatch")
	}
}

func TestLoadMasterKey_EnvFallback(t *testing.T) {
	key := make([]byte, 32)
	t.Setenv("RESLEEVE_SERVER_MASTER_KEY", hex.EncodeToString(key))
	got, err := loadMasterKey("") // empty file → env
	if err != nil {
		t.Fatalf("loadMasterKey: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("env load length = %d, want 32", len(got))
	}
}

func TestLoadMasterKey_AbsentIsNil(t *testing.T) {
	t.Setenv("RESLEEVE_SERVER_MASTER_KEY", "")
	got, err := loadMasterKey("")
	if err != nil {
		t.Fatalf("loadMasterKey: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil key when unset, got %d bytes", len(got))
	}
}

func TestLoadMasterKey_WrongLengthRejected(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "short.hex")
	if err := os.WriteFile(f, []byte(hex.EncodeToString([]byte("short"))), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMasterKey(f); err == nil {
		t.Fatal("loadMasterKey accepted a wrong-length key")
	}
}

func TestLoadMasterKey_GarbageRejected(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "junk")
	if err := os.WriteFile(f, []byte("!!! not hex or base64 !!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMasterKey(f); err == nil {
		t.Fatal("loadMasterKey accepted garbage")
	}
}
