package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

// TestOSKeychain_RoundTrip exercises Put/Get/Delete against the
// go-keyring in-process mock backend, so this test is hermetic and
// works on any CI runner (no real Keychain / Secret Service needed).
func TestOSKeychain_RoundTrip(t *testing.T) {
	keyring.MockInit()
	k := &osKeychain{file: &fileKeychain{root: t.TempDir()}}

	const (
		host    = "https://upstream.example"
		account = "alice@example.com"
		token   = "device-token-xyz"
	)

	// Absent → empty, no error.
	got, err := k.Get(host, account)
	if err != nil {
		t.Fatalf("Get on empty: %v", err)
	}
	if got != "" {
		t.Fatalf("Get on empty: want empty, got %q", got)
	}

	if err := k.Put(host, account, token); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err = k.Get(host, account)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != token {
		t.Fatalf("Get: want %q, got %q", token, got)
	}

	// Overwrite.
	if err := k.Put(host, account, "rotated"); err != nil {
		t.Fatalf("Put overwrite: %v", err)
	}
	got, _ = k.Get(host, account)
	if got != "rotated" {
		t.Fatalf("Get after overwrite: want %q, got %q", "rotated", got)
	}

	// Delete twice — idempotent.
	if err := k.Delete(host, account); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := k.Delete(host, account); err != nil {
		t.Fatalf("Delete (idempotent): %v", err)
	}
	got, err = k.Get(host, account)
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if got != "" {
		t.Fatalf("Get after delete: want empty, got %q", got)
	}
}

// TestOSKeychain_ServiceShardsByHost verifies two upstreams produce
// independent secrets even when the email account matches — important
// for users with work + personal upstreams.
func TestOSKeychain_ServiceShardsByHost(t *testing.T) {
	keyring.MockInit()
	k := &osKeychain{file: &fileKeychain{root: t.TempDir()}}
	if err := k.Put("https://a.example", "alice@example.com", "tok-a"); err != nil {
		t.Fatal(err)
	}
	if err := k.Put("https://b.example", "alice@example.com", "tok-b"); err != nil {
		t.Fatal(err)
	}
	gotA, _ := k.Get("https://a.example", "alice@example.com")
	gotB, _ := k.Get("https://b.example", "alice@example.com")
	if gotA != "tok-a" || gotB != "tok-b" {
		t.Fatalf("shard mismatch: a=%q b=%q", gotA, gotB)
	}
}

// TestFileKeychain_RoundTrip is the file-backend cousin of the OS
// round-trip — covers the fallback path on unsupported platforms.
func TestFileKeychain_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := &fileKeychain{root: dir}

	const (
		host    = "https://upstream.example"
		account = "bob@example.com"
		token   = "file-token"
	)

	got, err := f.Get(host, account)
	if err != nil {
		t.Fatalf("Get on empty: %v", err)
	}
	if got != "" {
		t.Fatalf("Get on empty: want empty, got %q", got)
	}

	if err := f.Put(host, account, token); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// File mode must be 0600 — but Unix permission bits are not enforced
	// on Windows (os.WriteFile's perm is largely ignored; Stat reports
	// 0666), so only assert this where the filesystem honors it.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(f.path(host, account))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("file mode: want 0600, got %v", mode)
		}
	}
	got, _ = f.Get(host, account)
	if got != token {
		t.Fatalf("Get: want %q, got %q", token, got)
	}

	if err := f.Delete(host, account); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := f.Delete(host, account); err != nil {
		t.Fatalf("Delete (idempotent): %v", err)
	}
}

// TestMaybeMigrateFromFile covers the one-shot import path: a token
// in the file backend → moved into the OS keychain on first
// `resleeve login` after upgrade, with the old file renamed to a
// `.migrated` suffix so it can't repeat.
func TestMaybeMigrateFromFile(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	fb := &fileKeychain{root: dir}
	k := &osKeychain{file: fb}

	const (
		host    = "https://upstream.example"
		account = "carol@example.com"
		token   = "legacy-token"
	)

	if err := fb.Put(host, account, token); err != nil {
		t.Fatalf("seed file backend: %v", err)
	}
	maybeMigrateFromFile(k, host, account)

	// Token should now be in the OS keychain.
	got, _ := k.Get(host, account)
	if got != token {
		t.Fatalf("post-migrate Get: want %q, got %q", token, got)
	}
	// Old file should be renamed.
	if _, err := os.Stat(fb.path(host, account)); !os.IsNotExist(err) {
		t.Fatalf("old file still present: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(fb.path(host, account)), "*.migrated"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("want 1 .migrated file, got %d (%v)", len(matches), matches)
	}

	// Second call is a no-op: nothing left in file backend, but should not
	// clobber the OS keychain value.
	maybeMigrateFromFile(k, host, account)
	got, _ = k.Get(host, account)
	if got != token {
		t.Fatalf("post-migrate (second call) Get: want %q, got %q", token, got)
	}
}

// TestMaybeMigrateFromFile_NoClobber: if the OS keychain already has
// a value, the migration must not overwrite it from a stale file.
func TestMaybeMigrateFromFile_NoClobber(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	fb := &fileKeychain{root: dir}
	k := &osKeychain{file: fb}

	const (
		host    = "https://upstream.example"
		account = "dave@example.com"
	)
	if err := fb.Put(host, account, "stale-file-token"); err != nil {
		t.Fatal(err)
	}
	if err := k.Put(host, account, "fresh-os-token"); err != nil {
		t.Fatal(err)
	}
	maybeMigrateFromFile(k, host, account)
	got, _ := k.Get(host, account)
	if got != "fresh-os-token" {
		t.Fatalf("clobbered fresh OS token: got %q", got)
	}
	// Stale file should be left untouched (no point renaming it; the
	// user already has a real OS keychain entry).
	if _, err := os.Stat(fb.path(host, account)); err != nil {
		t.Fatalf("file unexpectedly disappeared: %v", err)
	}
}

// TestMaybeMigrateFromFile_FileBackend is a no-op when the active
// backend is already the file backend (unsupported platform).
func TestMaybeMigrateFromFile_FileBackend(t *testing.T) {
	dir := t.TempDir()
	fb := &fileKeychain{root: dir}
	const (
		host    = "https://upstream.example"
		account = "eve@example.com"
	)
	if err := fb.Put(host, account, "tok"); err != nil {
		t.Fatal(err)
	}
	// Should not panic, should leave the file alone.
	maybeMigrateFromFile(fb, host, account)
	if _, err := os.Stat(fb.path(host, account)); err != nil {
		t.Fatalf("file disappeared: %v", err)
	}
}

// TestSafeName guards against path traversal / collisions caused by
// `/`, `:`, etc. in the host or account string.
func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"alice@example.com":           "alice@example.com",
		"https://upstream.example/v2": "https___upstream.example_v2",
		"with space":                  "with_space",
		"a/b\\c:d?e#f":                "a_b_c_d_e_f",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.ContainsAny(safeName("a/b"), "/\\") {
		t.Errorf("safeName leaked a separator")
	}
}
