package antigravity

import (
	"unicode/utf8"
)

// This file is a small, self-contained protobuf wire-format reader. It
// exists because Antigravity (Google Antigravity CLI, Windsurf/Cascade
// derived) persists each conversation as a SQLite database whose `steps`
// table stores a `step_payload` BLOB encoded as protobuf — with NO
// published .proto schema. Rather than pull in google.golang.org/protobuf
// (a heavy dep we deliberately avoid, see go.mod), we hand-roll just enough
// of the wire format to walk the message tree and harvest human-readable
// string leaves.
//
// Wire format reference (https://protobuf.dev/programming-guides/encoding):
//
//	tag      = (field_number << 3) | wire_type   (varint)
//	wire types: 0 varint, 1 fixed64, 2 length-delimited, 5 fixed32
//	            (3/4 group start/end are deprecated and not handled)
//
// We treat every length-delimited field as either (a) a nested message we
// recurse into, or (b) a leaf string when the bytes are valid UTF-8 and
// don't themselves parse as a plausible sub-message. This dual treatment is
// inherently heuristic (the wire format does not distinguish `string`,
// `bytes`, and nested-message — they all share wire type 2), so extraction
// is best-effort by design.

// wireType is a protobuf wire type (low 3 bits of a field tag).
type wireType int

const (
	wireVarint  wireType = 0
	wireFixed64 wireType = 1
	wireBytes   wireType = 2
	wireFixed32 wireType = 5
)

// stringLeaf is one recovered length-delimited field that we classify as a
// human-readable string. Path is the chain of field numbers from the root
// to this leaf (e.g. [3,1] = field 1 inside the message at field 3); it is
// retained so step classifiers can prefer leaves at known positions.
type stringLeaf struct {
	Path  []int
	Value string
}

// minLeafLen is the minimum length (bytes) a UTF-8 length-delimited field
// must have to be reported as a string leaf. Very short blobs (1-2 bytes)
// are almost always packed numbers/flags, not text, and add noise.
const minLeafLen = 3

// maxDepth bounds recursion so a malformed/adversarial blob cannot blow the
// stack. Real Antigravity payloads nest only a handful of levels.
const maxDepth = 24

// DecodeStrings walks a protobuf wire-format blob and returns every
// length-delimited field that decodes as a plausible human-readable string,
// in encounter order. It never errors: truncated/garbage input simply
// yields fewer (or zero) leaves. This is the single entry point used by the
// step→event mapper.
func DecodeStrings(buf []byte) []stringLeaf {
	var out []stringLeaf
	walk(buf, nil, 0, &out)
	return out
}

// walk parses one protobuf message body, appending string leaves to *out.
func walk(buf []byte, path []int, depth int, out *[]stringLeaf) {
	if depth > maxDepth {
		return
	}
	i := 0
	for i < len(buf) {
		tag, n := readVarint(buf[i:])
		if n == 0 {
			return // truncated tag — stop, keep what we have
		}
		i += n
		field := int(tag >> 3)
		wt := wireType(tag & 0x7)
		if field == 0 {
			return // field 0 is invalid; bail rather than spin
		}

		switch wt {
		case wireVarint:
			_, n := readVarint(buf[i:])
			if n == 0 {
				return
			}
			i += n
		case wireFixed64:
			if i+8 > len(buf) {
				return
			}
			i += 8
		case wireFixed32:
			if i+4 > len(buf) {
				return
			}
			i += 4
		case wireBytes:
			length, n := readVarint(buf[i:])
			if n == 0 {
				return
			}
			i += n
			if length > uint64(len(buf)-i) {
				return // length runs past the buffer — truncated
			}
			sub := buf[i : i+int(length)]
			i += int(length)
			childPath := appendPath(path, field)
			// A length-delimited field is ambiguous: nested message, string,
			// or raw bytes. Recurse to harvest any nested strings; ALSO, if
			// the bytes are themselves valid printable UTF-8, record them as
			// a leaf. We do both because a string field that happens to
			// contain bytes resembling a message would otherwise be lost,
			// and a message whose only useful content is strings is covered
			// by the recursion.
			if isProbablyMessage(sub) {
				walk(sub, childPath, depth+1, out)
			} else if s, ok := asString(sub); ok {
				*out = append(*out, stringLeaf{Path: childPath, Value: s})
			}
		default:
			return // groups (3/4) / unknown — stop parsing this message
		}
	}
}

// appendPath returns path with field appended, copied so callers don't
// share backing arrays across sibling recursions.
func appendPath(path []int, field int) []int {
	cp := make([]int, len(path)+1)
	copy(cp, path)
	cp[len(path)] = field
	return cp
}

// readVarint decodes a base-128 varint from the head of buf. Returns the
// value and the number of bytes consumed, or (0,0) if buf is truncated or
// the varint exceeds 10 bytes (malformed).
func readVarint(buf []byte) (uint64, int) {
	var v uint64
	var shift uint
	for i := 0; i < len(buf); i++ {
		b := buf[i]
		if i >= 10 {
			return 0, 0 // varint too long
		}
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, i + 1
		}
		shift += 7
	}
	return 0, 0 // ran off the end without a terminating byte
}

// isProbablyMessage reports whether sub looks like it can be parsed as a
// nested protobuf message: a full structural scan succeeds AND it consumes
// the whole buffer. We require a non-empty buffer; empty bytes are not a
// message worth recursing into.
func isProbablyMessage(sub []byte) bool {
	if len(sub) == 0 {
		return false
	}
	return scanMessage(sub)
}

// scanMessage validates that sub parses cleanly as a protobuf message
// (every field's wire type consumes exactly to the end of the buffer). It
// does not collect anything; it is the cheap structural predicate behind
// isProbablyMessage. A buffer that fails here is treated as a leaf.
func scanMessage(buf []byte) bool {
	i := 0
	fields := 0
	for i < len(buf) {
		tag, n := readVarint(buf[i:])
		if n == 0 {
			return false
		}
		i += n
		if int(tag>>3) == 0 {
			return false
		}
		switch wireType(tag & 0x7) {
		case wireVarint:
			_, n := readVarint(buf[i:])
			if n == 0 {
				return false
			}
			i += n
		case wireFixed64:
			if i+8 > len(buf) {
				return false
			}
			i += 8
		case wireFixed32:
			if i+4 > len(buf) {
				return false
			}
			i += 4
		case wireBytes:
			length, n := readVarint(buf[i:])
			if n == 0 {
				return false
			}
			i += n
			if length > uint64(len(buf)-i) {
				return false
			}
			i += int(length)
		default:
			return false
		}
		fields++
	}
	return i == len(buf) && fields > 0
}

// asString reports whether sub is a human-readable UTF-8 string worth
// reporting as a leaf, returning the trimmed text. We reject blobs that are
// too short, not valid UTF-8, or made mostly of control bytes (which would
// indicate packed numerics or binary, not text).
func asString(sub []byte) (string, bool) {
	if len(sub) < minLeafLen {
		return "", false
	}
	if !utf8.Valid(sub) {
		return "", false
	}
	printable := 0
	for _, r := range string(sub) {
		if r == '\n' || r == '\t' || r == '\r' || r >= 0x20 {
			printable++
		}
	}
	// Require the overwhelming majority of runes to be printable; a few
	// stray control bytes (rare) shouldn't disqualify otherwise good text,
	// but a blob that is mostly control bytes is binary, not a string.
	total := utf8.RuneCount(sub)
	if total == 0 || printable*10 < total*9 {
		return "", false
	}
	return string(sub), true
}
