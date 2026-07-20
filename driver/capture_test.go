package driver

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// makeWAV builds a minimal 16 kHz mono 16-bit PCM WAV from the given samples,
// so level tests can construct known-loud and known-silent inputs.
func makeWAV(samples []int16) []byte {
	const sampleRate = 16000
	dataLen := len(samples) * 2
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	for _, s := range samples {
		binary.Write(&b, binary.LittleEndian, s)
	}
	return b.Bytes()
}

func TestLoudEnough(t *testing.T) {
	cases := []struct {
		name string
		wav  []byte
		thr  float64
		want bool
	}{
		{"pure silence below threshold", makeWAV(make([]int16, 1600)), -50, false},
		{"full-scale above threshold", makeWAV([]int16{32767, -32768, 30000}), -50, true},
		// adversary: -32768 is full-scale (0 dBFS); a naive abs() leaves it negative.
		// At threshold 0.0 this also pins >= (not >).
		{"most-negative sample is full-scale, at threshold", makeWAV([]int16{-32768}), 0.0, true},
		{"quiet negative respects sign", makeWAV([]int16{-3277}), -30, true},
		{"moderate above lenient threshold", makeWAV([]int16{3277}), -30, true}, // ~-20 dBFS
		{"moderate below strict threshold", makeWAV([]int16{3277}), -10, false}, // ~-20 dBFS
		{"non-wav bytes", []byte("not a wav at all, just text"), -50, false},
		{"empty", nil, -50, false},
	}
	for _, c := range cases {
		if got := loudEnough(c.wav, c.thr); got != c.want {
			t.Errorf("%s: loudEnough(_, %v) = %v, want %v", c.name, c.thr, got, c.want)
		}
	}
}

// makeWAVWithJunkChunk puts a non-data chunk before "data" — to catch a parser
// that assumes data lives at a fixed offset instead of scanning chunks.
func makeWAVWithJunkChunk(samples []int16) []byte {
	const sampleRate = 16000
	dataLen := len(samples) * 2
	junk := []byte("junkpayload!")
	var b bytes.Buffer
	riffSize := 4 + (8 + 16) + (8 + len(junk)) + (8 + dataLen)
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(riffSize))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("JUNK")
	binary.Write(&b, binary.LittleEndian, uint32(len(junk)))
	b.Write(junk)
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	for _, s := range samples {
		binary.Write(&b, binary.LittleEndian, s)
	}
	return b.Bytes()
}

func TestLoudEnoughSkipsNonDataChunks(t *testing.T) {
	if !loudEnough(makeWAVWithJunkChunk([]int16{32767}), -6) {
		t.Fatal("loudEnough must find the data chunk after a non-data chunk, not assume a fixed offset")
	}
}

// --- SOP-3 helpers ---

func TestParseSilenceEnd(t *testing.T) {
	if got, ok := parseSilenceEnd("[silencedetect @ 0x1] silence_end: 3.2 | silence_duration: 1.7"); !ok || got != 3.2 {
		t.Fatalf("parseSilenceEnd = (%v, %v), want (3.2, true)", got, ok)
	}
	if _, ok := parseSilenceEnd("silence_start: 1.0"); ok {
		t.Fatal("silence_start is not a silence_end")
	}
	if _, ok := parseSilenceEnd("frame= 10"); ok {
		t.Fatal("unrelated line parsed as silence_end")
	}
}

func TestVadParser(t *testing.T) {
	ch := make(chan vadEvent, 8)
	p := &vadParser{out: ch}
	p.Write([]byte("silence_start: 1.5\n[silencedetect] silence_end: 3.2 | silence_duration: 1.7\n"))
	if e := <-ch; e.voiced || e.mediaT != 1.5 {
		t.Fatalf("first event = %+v, want silence_start @1.5 (voiced=false)", e)
	}
	if e := <-ch; !e.voiced || e.mediaT != 3.2 {
		t.Fatalf("second event = %+v, want silence_end @3.2 (voiced=true)", e)
	}
	// a line split across writes still parses
	p.Write([]byte("silence_st"))
	p.Write([]byte("art: 5\n"))
	if e := <-ch; e.voiced || e.mediaT != 5 {
		t.Fatalf("split-line event = %+v, want silence_start @5", e)
	}
}

func TestTrailingStopword(t *testing.T) {
	sw := []string{"done", "stop"}
	if !trailingStopword("okay pharmacy pickup after 8pm done", sw) {
		t.Error("trailing 'done' should match")
	}
	if !trailingStopword("check one two Stop.", sw) {
		t.Error("trailing 'Stop.' should match (case/punctuation)")
	}
	if trailingStopword("okay pharmacy pickup after 8pm", sw) {
		t.Error("no trailing stopword should not match")
	}
	if trailingStopword("", sw) {
		t.Error("empty text should not match")
	}
}

func TestParseSilenceStart(t *testing.T) {
	cases := []struct {
		name string
		line string
		want float64
		ok   bool
	}{
		// happy
		{"typical", "[silencedetect @ 0x7f] silence_start: 3.24", 3.24, true},
		// edge / boundary
		{"zero (leading silence)", "[silencedetect @ 0x7f] silence_start: 0", 0, true},
		{"surrounding whitespace", "   silence_start: 12.5  ", 12.5, true},
		// error / rejection
		{"silence_end is not a start", "[silencedetect @ 0x7f] silence_end: 5.1 | silence_duration: 1.8", 0, false},
		{"unrelated progress line", "frame=  100 fps= 30 q=-1.0 size=1kB", 0, false},
		{"malformed number", "silence_start: notanumber", 0, false},
		{"empty line", "", 0, false},
		// adversary gaps: ffmpeg-shaped lines
		{"keyword but no number", "silence_start:", 0, false},
		{"trailing pipe/token", "[silencedetect @ 0x7f] silence_start: 3.24 | silence_end: 5", 3.24, true},
		{"scientific notation", "silence_start: 1.5e-2", 0.015, true},
	}
	for _, c := range cases {
		got, ok := parseSilenceStart(c.line)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: parseSilenceStart(%q) = (%v, %v), want (%v, %v)", c.name, c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestEnvFloatOr(t *testing.T) {
	const key = "AATOOLKIT_TEST_FLOAT_XYZ"
	cases := []struct {
		name string
		set  bool
		val  string
		def  float64
		want float64
	}{
		{"unset returns default", false, "", 2.0, 2.0},
		{"valid float parsed", true, "3.5", 2.0, 3.5},
		{"integer-ish parsed", true, "8", 2.0, 8.0},
		{"empty string returns default", true, "", 2.0, 2.0},
		{"invalid returns default", true, "abc", 2.0, 2.0},
		// adversary gaps
		{"zero parses (not treated as unset)", true, "0", 2.0, 0.0},
		{"negative parses", true, "-1.5", 2.0, -1.5},
		{"whitespace-padded returns default", true, " 3.5 ", 2.0, 2.0},
	}
	for _, c := range cases {
		os.Unsetenv(key)
		if c.set {
			os.Setenv(key, c.val)
		}
		if got := EnvFloatOr(key, c.def); got != c.want {
			t.Errorf("%s: EnvFloatOr = %v, want %v", c.name, got, c.want)
		}
	}
	os.Unsetenv(key)
}
