package driver

import "testing"

// segmentAccumulator is the extracted, independently-testable form of the TTS
// segmentation rules that used to live only inside SendStream's byte loop:
// flush at a delimiter (. ? ! , \n) once the buffer has crossed the minimum
// length, keep the delimiter, never send a pure-whitespace segment, and force
// a final drain at end-of-stream regardless of length.

// A stream that crosses the minimum length at a delimiter must flush,
// keeping the delimiter, and leave the buffer empty afterward.
func TestSegmentAccumulator_FlushesAtDelimiterAboveThreshold(t *testing.T) {
	acc := newSegmentAccumulator(30)
	text := "The quick brown fox jumps over the lazy dog. "
	var segs []string
	for i := 0; i < len(text); i++ {
		if seg, ok := acc.write(text[i]); ok {
			segs = append(segs, seg)
		}
	}
	if len(segs) != 1 {
		t.Fatalf("want 1 segment, got %d: %v", len(segs), segs)
	}
	if segs[0] != "The quick brown fox jumps over the lazy dog." {
		t.Fatalf("segment mismatch: %q", segs[0])
	}
}

// A delimiter reached before the buffer crosses the minimum length must not
// flush yet.
func TestSegmentAccumulator_HoldsBelowThreshold(t *testing.T) {
	acc := newSegmentAccumulator(30)
	seg, ok := acc.write('.')
	if ok {
		t.Fatalf("expected no flush below threshold, got %q", seg)
	}
}

// flush() unconditionally drains the buffer (end-of-stream force flush),
// ignoring the length threshold.
func TestSegmentAccumulator_FlushDrainsRemainderBelowThreshold(t *testing.T) {
	acc := newSegmentAccumulator(30)
	acc.write('h')
	acc.write('i')
	seg, ok := acc.flush()
	if !ok || seg != "hi" {
		t.Fatalf("flush = %q, %v; want \"hi\", true", seg, ok)
	}
}

// A pure-whitespace buffer is never sent to onSegment, even on a forced
// flush.
func TestSegmentAccumulator_FlushSkipsWhitespaceOnly(t *testing.T) {
	acc := newSegmentAccumulator(30)
	acc.write(' ')
	acc.write('\t')
	acc.write('\n')
	seg, ok := acc.flush()
	if ok {
		t.Fatalf("expected no flush for whitespace-only buffer, got %q", seg)
	}
}

// After a flush, the buffer is reset — a subsequent write starts fresh.
func TestSegmentAccumulator_ResetsAfterFlush(t *testing.T) {
	acc := newSegmentAccumulator(3)
	acc.write('a')
	acc.write('b')
	acc.write('.') // len=3, >= min(3) -> flush "ab."
	acc.write('c')
	seg, ok := acc.flush()
	if !ok || seg != "c" {
		t.Fatalf("flush after reset = %q, %v; want \"c\", true", seg, ok)
	}
}

// Every rule-listed delimiter triggers a flush once past the threshold, not
// just '.'.
func TestSegmentAccumulator_AllDelimitersTrigger(t *testing.T) {
	for _, d := range []byte{'.', '?', '!', ','} {
		acc := newSegmentAccumulator(1)
		seg, ok := acc.write(d)
		if !ok {
			t.Fatalf("delimiter %q: expected flush, got none", d)
		}
		if len(seg) != 1 || seg[0] != d {
			t.Fatalf("delimiter %q: segment = %q, want the delimiter itself", d, seg)
		}
	}

	// '\n' alone is pure whitespace and is never flushed; pair it with a
	// non-whitespace byte first to exercise the delimiter behavior.
	acc := newSegmentAccumulator(1)
	acc.write('a')
	seg, ok := acc.write('\n')
	if !ok {
		t.Fatalf("delimiter %q: expected flush, got none", byte('\n'))
	}
	if seg != "a\n" {
		t.Fatalf("delimiter %q: segment = %q, want %q", byte('\n'), seg, "a\n")
	}
}
