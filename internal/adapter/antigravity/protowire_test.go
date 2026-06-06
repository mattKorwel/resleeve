package antigravity

import (
	"testing"
)

// --- minimal protobuf encoders for hand-crafting test blobs ---

func encVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func tag(field int, wt wireType) []byte {
	return encVarint(uint64(field)<<3 | uint64(wt))
}

// encBytes encodes a length-delimited field (wire type 2).
func encBytes(field int, val []byte) []byte {
	out := tag(field, wireBytes)
	out = append(out, encVarint(uint64(len(val)))...)
	return append(out, val...)
}

// encVarintField encodes a varint field (wire type 0).
func encVarintField(field int, v uint64) []byte {
	out := tag(field, wireVarint)
	return append(out, encVarint(v)...)
}

func encFixed64(field int, v uint64) []byte {
	out := tag(field, wireFixed64)
	for i := 0; i < 8; i++ {
		out = append(out, byte(v>>(8*i)))
	}
	return out
}

func TestDecodeStrings_TopLevelString(t *testing.T) {
	blob := encBytes(2, []byte("hello world"))
	leaves := DecodeStrings(blob)
	if len(leaves) != 1 {
		t.Fatalf("want 1 leaf, got %d: %+v", len(leaves), leaves)
	}
	if leaves[0].Value != "hello world" {
		t.Errorf("value = %q", leaves[0].Value)
	}
	if len(leaves[0].Path) != 1 || leaves[0].Path[0] != 2 {
		t.Errorf("path = %v, want [2]", leaves[0].Path)
	}
}

func TestDecodeStrings_Nested(t *testing.T) {
	// field 19 { field 2 = "prompt"  field 3 { field 1 = "echo" } }
	inner3 := encBytes(1, []byte("echo prompt text"))
	msg19 := append(encBytes(2, []byte("the user prompt")), encBytes(3, inner3)...)
	blob := encBytes(19, msg19)

	leaves := DecodeStrings(blob)
	if len(leaves) != 2 {
		t.Fatalf("want 2 leaves, got %d: %+v", len(leaves), leaves)
	}
	// First leaf: [19,2] = "the user prompt"
	if got := extractText(leaves, [][]int{{19, 2}}); got != "the user prompt" {
		t.Errorf("extractText [19,2] = %q", got)
	}
	// Second leaf: [19,3,1] = "echo prompt text"
	if got := firstAtPaths(leaves, [][]int{{19, 3, 1}}); got != "echo prompt text" {
		t.Errorf("firstAtPaths [19,3,1] = %q", got)
	}
}

func TestDecodeStrings_MixedWireTypes(t *testing.T) {
	// field 1 = varint 14 (step_type-ish), field 2 = string, field 7 = fixed64.
	var blob []byte
	blob = append(blob, encVarintField(1, 14)...)
	blob = append(blob, encBytes(2, []byte("tool args here"))...)
	blob = append(blob, encFixed64(7, 0xdeadbeef)...)

	leaves := DecodeStrings(blob)
	if len(leaves) != 1 {
		t.Fatalf("want 1 string leaf, got %d: %+v", len(leaves), leaves)
	}
	if leaves[0].Value != "tool args here" {
		t.Errorf("value = %q", leaves[0].Value)
	}
}

func TestDecodeStrings_RejectsShortAndBinary(t *testing.T) {
	// A 2-byte field is below minLeafLen; a control-byte field is binary.
	blob := encBytes(1, []byte("ab"))                          // too short
	blob = append(blob, encBytes(2, []byte{0, 1, 2, 3, 4})...) // binary
	leaves := DecodeStrings(blob)
	for _, l := range leaves {
		if l.Value == "ab" {
			t.Errorf("short string should be rejected")
		}
	}
}

func TestDecodeStrings_TruncatedIsLenient(t *testing.T) {
	// A valid string field followed by a truncated tag: should return the
	// good leaf and not panic.
	blob := encBytes(1, []byte("good value"))
	blob = append(blob, 0xFF) // dangling varint byte (high bit set, no continuation)
	leaves := DecodeStrings(blob)
	if len(leaves) != 1 || leaves[0].Value != "good value" {
		t.Fatalf("leaves = %+v", leaves)
	}
}

func TestReadVarint(t *testing.T) {
	cases := []struct {
		in  []byte
		val uint64
		n   int
	}{
		{[]byte{0x00}, 0, 1},
		{[]byte{0x01}, 1, 1},
		{[]byte{0x96, 0x01}, 150, 2},
		{[]byte{0xFF}, 0, 0}, // truncated
		{[]byte{}, 0, 0},     // empty
	}
	for _, c := range cases {
		v, n := readVarint(c.in)
		if v != c.val || n != c.n {
			t.Errorf("readVarint(%v) = (%d,%d), want (%d,%d)", c.in, v, n, c.val, c.n)
		}
	}
}

func TestDecodeStrings_NoStackBlowup(t *testing.T) {
	// Deeply nested message must not exceed maxDepth and must not panic.
	blob := []byte("leaf string deep")
	for i := 0; i < 200; i++ {
		blob = encBytes(1, blob)
	}
	_ = DecodeStrings(blob) // just must not panic / stack-overflow
}
