package telephony

import (
	"encoding/binary"
	"strconv"
	"testing"
)

// muLawGolden pins G.711 μ-law decoding to an external ground truth: these are
// the samples ffmpeg emits for the same input bytes
// (`ffmpeg -f mulaw -ar 8000 -ac 1 -i - -f s16le -acodec pcm_s16le -`).
// They are NOT re-derived from our own decoder, so a regression in
// muLawToLinear fails here instead of being asserted back at itself.
var muLawGolden = []struct {
	in   byte
	want int16
	name string
}{
	{0x00, -32124, "full-scale negative"},
	{0x80, 32124, "full-scale positive"},
	{0xFF, 0, "positive zero decodes to 0"},
	{0x7F, 0, "negative zero decodes to 0"},
	{0x54, -748, "mid-range negative"},
}

func TestMuLawDecodeMatchesG711(t *testing.T) {
	for _, tt := range muLawGolden {
		t.Run(tt.name, func(t *testing.T) {
			if got := muLawToLinear(tt.in); got != tt.want {
				t.Errorf("muLawToLinear(0x%02X) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// headerWant is one header field's expected value. Each implementation knows
// its own on-disk encoding, so the field's width is implied by its type
// instead of being restated as a number that could disagree with it.
type headerWant interface {
	// String renders the expectation.
	String() string
	// read decodes this field's bytes off the front of buf, rendering them
	// exactly as String renders the expectation — so the assertion is one
	// string comparison and the failure message reads naturally.
	read(buf []byte) string
}

// magic is a fixed ASCII chunk/format ID, stored as its own characters.
// It renders quoted so a trailing space (as in "fmt ") stays visible.
type magic string

func (m magic) String() string         { return strconv.Quote(string(m)) }
func (m magic) read(buf []byte) string { return magic(buf[:len(m)]).String() }

// le16 and le32 are little-endian unsigned integers of 2 and 4 bytes.
type le16 uint16

func (v le16) String() string         { return strconv.FormatUint(uint64(v), 10) }
func (v le16) read(buf []byte) string { return le16(binary.LittleEndian.Uint16(buf)).String() }

type le32 uint32

func (v le32) String() string         { return strconv.FormatUint(uint64(v), 10) }
func (v le32) read(buf []byte) string { return le32(binary.LittleEndian.Uint32(buf)).String() }

// headerField describes one fixed-position field of the 44-byte WAV/RIFF
// header: where it starts and what it should hold.
type headerField struct {
	name   string
	offset int
	want   headerWant
}

// ByteRate = SampleRate * NumChannels * BitsPerSample/8 = 8000 * 1 * 2.
// BlockAlign = NumChannels * BitsPerSample/8 = 2.
var wavHeaderFields = []headerField{
	{"RIFF magic", 0, magic("RIFF")},
	{"ChunkSize", 4, le32(36 + 2000)},
	{"WAVE magic", 8, magic("WAVE")},
	{"fmt ID", 12, magic("fmt ")},
	{"Subchunk1Size", 16, le32(16)},
	{"AudioFormat", 20, le16(1)},
	{"NumChannels", 22, le16(1)},
	{"SampleRate", 24, le32(8000)},
	{"ByteRate", 28, le32(16000)},
	{"BlockAlign", 32, le16(2)},
	{"BitsPerSample", 34, le16(16)},
	{"data ID", 36, magic("data")},
	{"Subchunk2Size", 40, le32(2000)},
}

func TestMulawToWAV_Header(t *testing.T) {
	wav := mulawToWAV(make([]byte, 1000), 8000)

	if len(wav) != 44+2000 {
		t.Fatalf("WAV length = %d, want %d", len(wav), 44+2000)
	}

	for _, f := range wavHeaderFields {
		if got := f.want.read(wav[f.offset:]); got != f.want.String() {
			t.Errorf("%s = %s, want %s", f.name, got, f.want)
		}
	}
}

func TestMulawToWAV_PCMData(t *testing.T) {
	in := make([]byte, 0, len(muLawGolden))
	for _, g := range muLawGolden {
		in = append(in, g.in)
	}

	wav := mulawToWAV(in, 8000)

	for i, g := range muLawGolden {
		off := 44 + i*2
		got := int16(binary.LittleEndian.Uint16(wav[off : off+2]))
		if got != g.want {
			t.Errorf("PCM[%d] (0x%02X) = %d, want %d", i, g.in, got, g.want)
		}
	}
}

func TestMulawToWAV_Empty(t *testing.T) {
	wav := mulawToWAV(nil, 8000)

	if len(wav) != 44 {
		t.Fatalf("empty clip should still emit a 44-byte header, got %d bytes", len(wav))
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != 0 {
		t.Errorf("Subchunk2Size = %d, want 0", got)
	}
}
