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
		// Encode the prefix the same way path() encodes keys so the walk
		// roots at the real on-disk directory. Trailing '/' is dropped so
		// the final segment isn't treated as an empty one.
		startDir = filepath.Join(b.root, filepath.FromSlash(encodeKey(strings.TrimSuffix(prefix, "/"))))
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
		key := decodeKey(strings.TrimSuffix(filepath.ToSlash(rel), ext))
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
	return filepath.Join(b.root, filepath.FromSlash(encodeKey(key))+ext)
}

// Keys are arbitrary '/'-separated strings (ValidateKey permits any
// segment content, and encodeScopePath maps scope '/' to ':'), but some
// characters that are valid in a key segment are illegal in a filename on
// Windows (notably ':', plus '<>"\|?*' and control bytes). encodeKey
// percent-encodes those characters — and '%' itself — within each
// segment so a key maps to a filename valid on every supported OS. '/'
// separators are preserved (they map to subdirectories). The transform is
// applied uniformly on all platforms so the on-disk layout is identical
// everywhere, and decodeKey reverses it exactly for List.
func encodeKey(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = encodeSegment(s)
	}
	return strings.Join(segs, "/")
}

// decodeKey reverses encodeKey, reconstructing the original key from a
// '/'-joined sequence of encoded segments.
func decodeKey(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = decodeSegment(s)
	}
	return strings.Join(segs, "/")
}

// reservedInSegment reports whether c must be percent-encoded to be a
// portable filename byte: the Windows-reserved set plus ASCII control
// bytes. '/' is never passed here (segments are split on it).
func reservedInSegment(c byte) bool {
	switch c {
	case '<', '>', ':', '"', '\\', '|', '?', '*':
		return true
	}
	return c < 0x20
}

const upperHex = "0123456789ABCDEF"

func encodeSegment(seg string) string {
	needs := false
	for i := 0; i < len(seg); i++ {
		if seg[i] == '%' || reservedInSegment(seg[i]) {
			needs = true
			break
		}
	}
	if !needs {
		return seg
	}
	var b strings.Builder
	b.Grow(len(seg) + 8)
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c == '%' || reservedInSegment(c) {
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func decodeSegment(seg string) string {
	if !strings.Contains(seg, "%") {
		return seg
	}
	var b strings.Builder
	b.Grow(len(seg))
	for i := 0; i < len(seg); i++ {
		if seg[i] == '%' && i+2 < len(seg) {
			if hi, lo := unhex(seg[i+1]), unhex(seg[i+2]); hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(seg[i])
	}
	return b.String()
}

func unhex(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}
