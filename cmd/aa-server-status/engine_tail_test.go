package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// countingReaderAt wraps an io.ReaderAt and records how many bytes were read.
// tailLines' contract is that it tails a large log without reading the whole
// file, so tests assert on the byte count.
type countingReaderAt struct {
	ra   io.ReaderAt
	read int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.ra.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}

func manyLines(count int) []byte {
	var sb strings.Builder
	for i := range count {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	return []byte(sb.String())
}

// --- happy path / large file ----------------------------------------------

// A file far larger than one chunk still returns exactly the last n lines.
func TestTailLines_LargeFileReturnsLastN(t *testing.T) {
	data := manyLines(100_000) // ~1 MB, many chunks
	lines, err := tailLines(bytes.NewReader(data), int64(len(data)), 50, false)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
	if lines[0] != "line 99950" {
		t.Errorf("first line = %q, want %q", lines[0], "line 99950")
	}
	if lines[49] != "line 99999" {
		t.Errorf("last line = %q, want %q", lines[49], "line 99999")
	}
}

// --- the core resource contract -------------------------------------------

// Tailing a multi-MB log must NOT read the whole file — only enough from the
// end to find the last n lines.
func TestTailLines_DoesNotReadWholeFile(t *testing.T) {
	data := manyLines(200_000) // ~2 MB
	c := &countingReaderAt{ra: bytes.NewReader(data)}
	lines, err := tailLines(c, int64(len(data)), 50, false)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if lines[len(lines)-1] != "line 199999" {
		t.Errorf("last line = %q, want %q", lines[len(lines)-1], "line 199999")
	}
	// The last 50 short lines occupy well under 1 KB; a bounded tail should
	// read a tiny fraction of the ~2 MB file. Allow generous chunking slack.
	if c.read > int64(len(data))/4 {
		t.Errorf("read %d of %d bytes — not a bounded tail", c.read, len(data))
	}
}

// --- UTF-8 across chunk boundaries ----------------------------------------

// A line of multi-byte runes longer than several chunks must decode intact:
// chunk boundaries (powers of two) fall mid-rune, so an implementation that
// decoded each chunk independently would corrupt runes. tailLines must
// reassemble bytes across chunk boundaries before decoding.
func TestTailLines_MultibyteRunesAcrossChunks(t *testing.T) {
	line := strings.Repeat("世界", 5_000) // 20 KB of 3-byte runes, spans many chunks
	data := []byte(line + "\n")
	lines, err := tailLines(bytes.NewReader(data), int64(len(data)), 50, false)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if lines[0] != line {
		t.Errorf("multibyte content corrupted across chunk boundaries")
	}
	if strings.ContainsRune(lines[0], utf8.RuneError) {
		t.Errorf("replacement char present — a rune was split across a chunk boundary")
	}
}

// nowrap truncation counts runes, not bytes, even for multi-byte content.
func TestTailLines_NowrapCountsRunesNotBytes(t *testing.T) {
	line := strings.Repeat("世", 100) // 100 runes, 300 bytes
	data := []byte(line + "\n")
	lines, err := tailLines(bytes.NewReader(data), int64(len(data)), 50, true)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if got := utf8.RuneCountInString(lines[0]); got != viewWrapWidth {
		t.Errorf("nowrap truncated to %d runes, want %d", got, viewWrapWidth)
	}
}

// --- parity with the previous whole-file semantics -------------------------

// Trailing empty lines (from the final newline and any blank tail) are stripped.
func TestTailLines_StripsTrailingEmptyLines(t *testing.T) {
	data := []byte("a\nb\nc\n\n\n")
	lines, err := tailLines(bytes.NewReader(data), int64(len(data)), 50, false)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if strings.Join(lines, ",") != "a,b,c" {
		t.Errorf("got %q, want [a b c]", lines)
	}
}

// An empty source yields no lines and no error.
func TestTailLines_Empty(t *testing.T) {
	lines, err := tailLines(bytes.NewReader(nil), 0, 50, false)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("want 0 lines, got %d: %v", len(lines), lines)
	}
}

// A source with no trailing newline still returns its final line.
func TestTailLines_NoTrailingNewline(t *testing.T) {
	data := []byte("first\nsecond")
	lines, err := tailLines(bytes.NewReader(data), int64(len(data)), 50, false)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(lines) != 2 || lines[1] != "second" {
		t.Errorf("got %v, want [first second]", lines)
	}
}

// Fewer than n lines → all lines returned, no panic.
func TestTailLines_FewerThanN(t *testing.T) {
	data := []byte("1\n2\n3\n")
	lines, err := tailLines(bytes.NewReader(data), int64(len(data)), 50, false)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(lines) != 3 {
		t.Errorf("want 3 lines, got %d", len(lines))
	}
}

// The last n lines spanning several backward chunks must come back in order.
// Short-line fixtures fit the whole tail in one chunk and so never exercise
// cross-chunk assembly; here each line is ~1 KB so 50 lines span many chunks.
func TestTailLines_TailSpansMultipleChunks(t *testing.T) {
	pad := strings.Repeat("x", 1000)
	var sb strings.Builder
	for i := range 300 {
		fmt.Fprintf(&sb, "line %d %s\n", i, pad) // ~1 KB/line → tail of 50 ≈ 50 KB
	}
	data := []byte(sb.String())
	lines, err := tailLines(bytes.NewReader(data), int64(len(data)), 50, false)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
	for i, l := range lines {
		want := fmt.Sprintf("line %d %s", 250+i, pad)
		if l != want {
			t.Fatalf("line %d out of order/corrupted: got prefix %q", i, l[:min(20, len(l))])
		}
	}
}

// A line straddling the exact n-boundary of a chunk: the (n+1)-th-from-last
// line must be excluded even when it sits in the same chunk as the kept tail.
func TestTailLines_BoundaryExactlyN(t *testing.T) {
	data := manyLines(51) // lines "line 0".."line 50"
	lines, err := tailLines(bytes.NewReader(data), int64(len(data)), 50, false)
	if err != nil {
		t.Fatalf("tailLines: %v", err)
	}
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
	if lines[0] != "line 1" {
		t.Errorf("first line = %q, want %q (line 0 must be dropped)", lines[0], "line 1")
	}
}
