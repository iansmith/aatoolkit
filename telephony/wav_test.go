package telephony

import (
	"encoding/binary"
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

// headerField describes one fixed-position field of the 44-byte WAV/RIFF
// header: its byte range and expected value. want is a string for the four
// four-character magic/ID fields, uint32 or uint16 for the rest — matching
// each field's on-disk width.
type headerField struct {
	name   string
	offset int
	width  int
	want   any
}

// ByteRate = SampleRate * NumChannels * BitsPerSample/8 = 8000 * 1 * 2.
// BlockAlign = NumChannels * BitsPerSample/8 = 2.
var wavHeaderFields = []headerField{
	{"RIFF magic", 0, 4, "RIFF"},
	{"ChunkSize", 4, 4, uint32(36 + 2000)},
	{"WAVE magic", 8, 4, "WAVE"},
	{"fmt ID", 12, 4, "fmt "},
	{"Subchunk1Size", 16, 4, uint32(16)},
	{"AudioFormat", 20, 2, uint16(1)},
	{"NumChannels", 22, 2, uint16(1)},
	{"SampleRate", 24, 4, uint32(8000)},
	{"ByteRate", 28, 4, uint32(16000)},
	{"BlockAlign", 32, 2, uint16(2)},
	{"BitsPerSample", 34, 2, uint16(16)},
	{"data ID", 36, 4, "data"},
	{"Subchunk2Size", 40, 4, uint32(2000)},
}

func TestMulawToWAV_Header(t *testing.T) {
	wav := mulawToWAV(make([]byte, 1000), 8000)

	if len(wav) != 44+2000 {
		t.Fatalf("WAV length = %d, want %d", len(wav), 44+2000)
	}

	for _, f := range wavHeaderFields {
		raw := wav[f.offset : f.offset+f.width]
		switch want := f.want.(type) {
		case string:
			if got := string(raw); got != want {
				t.Errorf("%s = %q, want %q", f.name, got, want)
			}
		case uint32:
			if got := binary.LittleEndian.Uint32(raw); got != want {
				t.Errorf("%s = %d, want %d", f.name, got, want)
			}
		case uint16:
			if got := binary.LittleEndian.Uint16(raw); got != want {
				t.Errorf("%s = %d, want %d", f.name, got, want)
			}
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
