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

func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
