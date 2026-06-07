package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadEndpoint reads the daemon's endpoint URL and shared secret.
// Resolution order:
//  1. $RESLEEVE_AGENT_ENDPOINT env var (URL); pairs with $RESLEEVE_AGENT_SECRET if set.
//  2. The endpoint file at EndpointPath() containing "URL\nSECRET\n".
// Returns an error if no endpoint can be resolved.
func LoadEndpoint() (url, secret string, err error) {
	if e := os.Getenv("RESLEEVE_AGENT_ENDPOINT"); e != "" {
		return e, os.Getenv("RESLEEVE_AGENT_SECRET"), nil
	}
	path, err := EndpointPath()
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read endpoint file %s: %w", path, err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	url = strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		secret = strings.TrimSpace(lines[1])
	}
	return url, secret, nil
}

// WriteEndpoint writes the daemon URL + shared secret to the endpoint
// file at EndpointPath(). Returns the resolved path. Atomic via
// tempfile + rename.
func WriteEndpoint(url, secret string) (string, error) {
	path, err := EndpointPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	body := url + "\n" + secret + "\n"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("rename: %w", err)
	}
	return path, nil
}

// RemoveEndpoint deletes the endpoint file. Idempotent.
func RemoveEndpoint(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// EndpointPath returns the platform-appropriate path for the endpoint
// file. Order: $XDG_RUNTIME_DIR/resleeve.endpoint (Linux), else
// ~/.resleeve/endpoint (macOS / Windows fallback).
func EndpointPath() (string, error) {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "resleeve.endpoint"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("homedir: %w", err)
	}
	return filepath.Join(home, ".resleeve", "endpoint"), nil
}

// DataDir returns the directory where resleeve stores its data
// (database, blobs, pid file, daemon log). ~/.resleeve/ today.
func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("homedir: %w", err)
	}
	return filepath.Join(home, ".resleeve"), nil
}

// PIDPath returns the path to the daemon PID file (under DataDir).
func PIDPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.pid"), nil
}

// LogPath returns the path to the daemon log file (under DataDir).
func LogPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}

// ActiveBrainPath returns the path to the file recording the globally
// selected "active brain" (under DataDir). When the file is present and
// non-empty, the daemon's upstream sync appends ?brain=<id> to its
// push/pull/SSE calls so the machine acts in that shared brain instead of
// the user's personal brain. Absent/empty = personal brain (the server
// default). This is a GLOBAL selection for round-11b; per-scope routing
// is a later refinement (see docs/design/round-11/05-sharing-slice.md).
func ActiveBrainPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "active_brain"), nil
}

// LoadActiveBrain returns the persisted active-brain id, or "" when none
// is set (file absent or empty). A read error other than "not exist" is
// returned so callers can surface a corrupt config rather than silently
// defaulting.
func LoadActiveBrain() (string, error) {
	path, err := ActiveBrainPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read active-brain file %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteActiveBrain persists the active-brain id atomically (tempfile +
// rename). An empty id clears the selection (removes the file), reverting
// the machine to its personal brain.
func WriteActiveBrain(brainID string) error {
	path, err := ActiveBrainPath()
	if err != nil {
		return err
	}
	brainID = strings.TrimSpace(brainID)
	if brainID == "" {
		return RemoveEndpoint(path) // reuse the idempotent-remove helper
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(brainID+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
