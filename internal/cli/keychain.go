package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	keyring "github.com/zalando/go-keyring"

	"github.com/mattkorwel/resleeve/internal/agent"
)

// keychain is the abstraction for storing the per-device bearer token
// returned by `resleeve serve` /v2/auth/login. The token gates ALL
// /v2/sync/* traffic for this device — losing it == reauth required.
//
// Round-5 follow-up A: the default backend is now the OS-native secret
// store — macOS Keychain on darwin, libsecret (Secret Service) on
// linux, Windows Credential Manager on windows — via
// github.com/zalando/go-keyring. On unsupported platforms we fall back
// to the historical 0600 file backend with a one-line stderr warning,
// so the binary still works in headless environments (CI containers,
// stripped Linux boxes without a Secret Service provider).
//
// The interface is intentionally minimal so swapping backends stays
// single-package.
//
// Known limitations of the file fallback (called out in the
// punch-list/learnings):
//   - readable by any process running as the same user
//   - survives logout unless explicitly purged
//   - does not benefit from secure enclave / kernel keyring isolation
//
// These match the same threat model as ~/.resleeve/seal.key and the
// agent endpoint secret today, so file-based remains a coherent
// emergency fallback.
type keychain interface {
	// Put stores token under (host, account). Overwrites prior values.
	Put(host, account, token string) error
	// Get returns the token for (host, account), or "" if absent.
	Get(host, account string) (string, error)
	// Delete removes the (host, account) entry. Idempotent.
	Delete(host, account string) error
}

// keychainService is the service-name prefix used in the OS keychain.
// Real entries are keyed as "<keychainService>:<host>" — that way a
// user with two upstreams (work + personal) sees two distinct items
// in Keychain Access / seahorse rather than collisions on a shared
// "account" field. account = the user's email.
const keychainService = "resleeve"

// defaultKeychain returns the keychain to use for this build. When the
// OS-native backend is unavailable, we log a single warning to stderr
// and fall back to the file backend so the CLI still works.
func defaultKeychain() (keychain, error) {
	dir, err := agent.DataDir()
	if err != nil {
		return nil, err
	}
	fb := &fileKeychain{root: filepath.Join(dir, "devices")}
	if osKeychainSupported() {
		return &osKeychain{file: fb}, nil
	}
	fmt.Fprintf(os.Stderr,
		"resleeve: OS keychain unavailable on %s; falling back to %s (mode 0600).\n",
		runtime.GOOS, fb.root)
	return fb, nil
}

// osKeychainSupported reports whether github.com/zalando/go-keyring
// has a real backend for the current GOOS. The library returns
// keyring.ErrUnsupportedPlatform on platforms it doesn't handle (e.g.
// freebsd, plan9). On Linux the call may still fail at runtime if no
// Secret Service provider is running — that error surfaces from Put/
// Get/Delete and is handled inline.
func osKeychainSupported() bool {
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		return true
	default:
		return false
	}
}

// osKeychain stores tokens in the OS-native secret store. It keeps a
// reference to the file backend (`file`) purely for the one-shot
// migration import performed by maybeMigrateFromFile — never for new
// writes.
type osKeychain struct {
	file *fileKeychain
}

// serviceFor returns the per-host service string used as the OS
// keychain "service" field. We embed the host so users see meaningful
// entries in Keychain Access etc., and so listing/deleting by service
// matches a single upstream.
func serviceFor(host string) string {
	return keychainService + ":" + host
}

func (k *osKeychain) Put(host, account, token string) error {
	if err := keyring.Set(serviceFor(host), account, token); err != nil {
		return fmt.Errorf("keychain set: %w", err)
	}
	return nil
}

func (k *osKeychain) Get(host, account string) (string, error) {
	v, err := keyring.Get(serviceFor(host), account)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("keychain get: %w", err)
	}
	return v, nil
}

func (k *osKeychain) Delete(host, account string) error {
	if err := keyring.Delete(serviceFor(host), account); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("keychain delete: %w", err)
	}
	return nil
}

// fileKeychain persists tokens under <root>/<host>/<account>.json with
// mode 0600. Files are sharded by host so a single `serve` deployment
// with multiple accounts (rare) doesn't conflict. This is the
// fallback path on unsupported platforms and the source for the
// one-shot migration import in maybeMigrateFromFile.
type fileKeychain struct{ root string }

type devEntry struct {
	Token  string `json:"token"`
	UserID string `json:"user_id,omitempty"`
}

func (f *fileKeychain) path(host, account string) string {
	return filepath.Join(f.root, safeName(host), safeName(account)+".json")
}

func (f *fileKeychain) Put(host, account, token string) error {
	p := f.path(host, account)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("keychain mkdir: %w", err)
	}
	body, err := json.Marshal(devEntry{Token: token})
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("keychain write: %w", err)
	}
	return os.Rename(tmp, p)
}

func (f *fileKeychain) Get(host, account string) (string, error) {
	body, err := os.ReadFile(f.path(host, account))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	var e devEntry
	if err := json.Unmarshal(body, &e); err != nil {
		return "", fmt.Errorf("keychain decode: %w", err)
	}
	return e.Token, nil
}

func (f *fileKeychain) Delete(host, account string) error {
	if err := os.Remove(f.path(host, account)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// maybeMigrateFromFile is the one-shot upgrade hook for users coming
// from the pre-follow-up-A file backend. On first `resleeve login`
// after the upgrade, for the (upstream, email) we're about to log
// into, if a token exists in the file backend AND nothing is in the
// OS keychain yet, copy it across and rename the file with a `.bak`
// suffix so a re-run won't re-migrate (and so the user can audit).
//
// Errors here are non-fatal: migration is a convenience, the login
// will overwrite the OS-keychain entry anyway. We just emit a short
// stderr note so the user knows it happened.
func maybeMigrateFromFile(kc keychain, host, account string) {
	osk, ok := kc.(*osKeychain)
	if !ok {
		// Already on file backend — nothing to migrate.
		return
	}
	// If the OS keychain already has an entry, don't clobber it.
	existing, err := osk.Get(host, account)
	if err == nil && existing != "" {
		return
	}
	tok, err := osk.file.Get(host, account)
	if err != nil || tok == "" {
		return
	}
	if err := osk.Put(host, account, tok); err != nil {
		fmt.Fprintln(os.Stderr, "keychain migrate: import failed:", err)
		return
	}
	// Rename rather than delete so a paranoid user can roll back.
	old := osk.file.path(host, account)
	bak := old + ".migrated"
	if err := os.Rename(old, bak); err != nil {
		fmt.Fprintln(os.Stderr, "keychain migrate: rename old file failed:", err)
		return
	}
	fmt.Fprintf(os.Stderr,
		"resleeve: migrated device token from %s to OS keychain (old file renamed to %s).\n",
		old, filepath.Base(bak))
}

// safeName makes a string filesystem-safe by replacing path separators
// and other reserved chars with '_'. We don't need reversibility — the
// (host, account) pair is also stored inside the JSON.
func safeName(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "?", "_", "#", "_", " ", "_")
	return r.Replace(s)
}
