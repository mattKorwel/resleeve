// Package sync defines the storage abstraction used by `resleeve serve`
// to persist encrypted session and memory blobs uploaded by clients.
//
// Backends are pluggable behind a stable interface (see Backend); the
// `local` subpackage implements a filesystem-backed backend used in v2
// MVP for self-hosting on a single VPS or homeserver. Object-store and
// git backends slot in alongside per docs/design/round-4/02-cross-machine-sync.md.
//
// **Zero-knowledge contract**: the server stores opaque ciphertext.
// Backends never decrypt, never inspect content, never reorder. Clients
// hold the KEK; backends just persist bytes keyed by an opaque cursor
// space (described below).
package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned by Backend.Get when the key is absent.
var ErrNotFound = errors.New("sync: not found")

// Backend is the interface every storage backend implements. Keys are
// opaque to the backend; callers are responsible for ensuring keys
// encode the (uid, kind, id, seq) tuple in a way that preserves
// lexicographic ordering on List.
type Backend interface {
	// Put stores a blob under key. Put is idempotent on identical (key,
	// blob) pairs — re-Put with the same content is a no-op success.
	// Re-Put with the same key but different content is a caller bug
	// (keys must include seq so collisions can't happen in normal
	// operation); implementations may return an error or overwrite, at
	// their discretion.
	Put(ctx context.Context, key string, blob []byte) error

	// Get returns the blob at key, or ErrNotFound if absent.
	Get(ctx context.Context, key string) ([]byte, error)

	// List returns the keys under prefix whose lexicographic ordering
	// is strictly greater than sinceCursor. The slice is sorted
	// ascending. nextCursor (empty when the page is the last one)
	// should be passed as sinceCursor on the next call to paginate.
	// limit caps the page size; a backend may return fewer.
	List(ctx context.Context, prefix string, sinceCursor string, limit int) (keys []string, nextCursor string, err error)

	// Delete removes a key. Used by retention; never by sync logic
	// itself. ErrNotFound is acceptable on a missing key.
	Delete(ctx context.Context, key string) error
}

// ValidateKey returns nil if the key is well-formed for use across all
// Backend implementations: relative path components separated by '/',
// no leading or trailing '/', no '.' or '..' segments, no empty
// segments. This is the universal floor — individual backends may
// impose stricter rules.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("sync: empty key")
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("sync: key %q has leading or trailing slash", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" {
			return fmt.Errorf("sync: key %q has empty segment", key)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("sync: key %q has dot segment", key)
		}
	}
	return nil
}
