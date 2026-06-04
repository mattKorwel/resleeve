// Package local implements a filesystem-backed sync.Backend for v2 MVP
// self-hosting. Blobs are stored as files under a root directory; keys
// are mapped to relative file paths with a ".enc" suffix (the file
// content is opaque ciphertext from the client's perspective —
// "encrypted" is the caller's contract; this backend just stores bytes).
//
// Cursor format: the cursor is the key itself. List returns keys
// strictly greater than the cursor in lexicographic order. Pagination
// is naturally restartable: the last returned key becomes the cursor
// for the next call.
package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mattkorwel/resleeve/internal/sync"
)

// ext is the suffix every stored blob file carries. Used to filter out
// junk if a user drops other files into the root.
const ext = ".enc"

// Backend stores blobs under root as <root>/<key>.enc.
type Backend struct {
	root string
}

// New creates a Backend rooted at the given directory. The directory
// is created (with parents) if it does not exist.
func New(root string) (*Backend, error) {
	if root == "" {
		return nil, fmt.Errorf("local backend: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("local backend: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("local backend: mkdir %s: %w", abs, err)
	}
	return &Backend{root: abs}, nil
}

// Put writes the blob to <root>/<key>.enc atomically via tempfile +
// rename. Parent directories are created as needed.
func (b *Backend) Put(ctx context.Context, key string, blob []byte) error {
	if err := sync.ValidateKey(key); err != nil {
		return err
	}
	full := b.path(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("local backend: mkdir %s: %w", filepath.Dir(full), err)
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("local backend: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, full); err != nil {
		return fmt.Errorf("local backend: rename: %w", err)
	}
	return nil
}

// Get reads the blob at <root>/<key>.enc. Returns sync.ErrNotFound
// when the file does not exist.
func (b *Backend) Get(ctx context.Context, key string) ([]byte, error) {
	if err := sync.ValidateKey(key); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(b.path(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, sync.ErrNotFound
		}
		return nil, fmt.Errorf("local backend: read %s: %w", b.path(key), err)
	}
	return data, nil
}

// List walks the tree under <root>/<prefix>, returning keys strictly
// greater than sinceCursor in lexicographic order, up to limit. An
// empty prefix walks the entire root.
func (b *Backend) List(ctx context.Context, prefix string, sinceCursor string, limit int) ([]string, string, error) {
	if prefix != "" {
		if err := sync.ValidateKey(strings.TrimSuffix(prefix, "/")); err != nil {
			return nil, "", err
		}
	}
	if limit <= 0 {
		limit = 100
	}

	var startDir string
	if prefix == "" {
		startDir = b.root
	} else {
		startDir = filepath.Join(b.root, prefix)
	}

	var keys []string
	err := filepath.Walk(startDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				// Prefix dir doesn't exist yet — empty result, not an error.
				return filepath.SkipDir
			}
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ext) {
			return nil
		}
		rel, err := filepath.Rel(b.root, path)
		if err != nil {
			return err
		}
		key := strings.TrimSuffix(filepath.ToSlash(rel), ext)
		if key > sinceCursor {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("local backend: walk: %w", err)
	}

	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	next := ""
	if len(keys) == limit {
		next = keys[len(keys)-1]
	}
	return keys, next, nil
}

// Delete removes the file at <root>/<key>.enc. Returns nil if absent
// (idempotent).
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := sync.ValidateKey(key); err != nil {
		return err
	}
	if err := os.Remove(b.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("local backend: remove %s: %w", b.path(key), err)
	}
	return nil
}

// Root returns the absolute root directory. Useful for tests and doctor output.
func (b *Backend) Root() string { return b.root }

func (b *Backend) path(key string) string {
	return filepath.Join(b.root, filepath.FromSlash(key)+ext)
}
