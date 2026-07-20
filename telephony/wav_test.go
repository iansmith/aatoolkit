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
	{0xFF, 0, "negative zero decodes to 0"},
	{0x7F, 0, "positive zero decodes to 0"},
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

func TestMulawToWAV_Header(t *testing.T) {
	wav := mulawToWAV(make([]byte, 1000), 8000)

	if len(wav) != 44+2000 {
		t.Fatalf("WAV length = %d, want %d", len(wav), 44+2000)
	}

	if got := string(wav[0:4]); got != "RIFF" {
		t.Errorf("RIFF magic = %q", got)
	}
	if got := binary.LittleEndian.Uint32(wav[4:8]); got != 36+2000 {
		t.Errorf("ChunkSize = %d, want %d", got, 36+2000)
	}
	if got := string(wav[8:12]); got != "WAVE" {
		t.Errorf("WAVE magic = %q", got)
	}
	if got := string(wav[12:16]); got != "fmt " {
		t.Errorf("fmt ID = %q", got)
	}
	if got := binary.LittleEndian.Uint32(wav[16:20]); got != 16 {
		t.Errorf("Subchunk1Size = %d, want 16", got)
	}
	if got := binary.LittleEndian.Uint16(wav[20:22]); got != 1 {
		t.Errorf("AudioFormat = %d, want 1 (PCM)", got)
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Errorf("NumChannels = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 8000 {
		t.Errorf("SampleRate = %d, want 8000", got)
	}
	// ByteRate = SampleRate * NumChannels * BitsPerSample/8 = 8000 * 1 * 2.
	if got := binary.LittleEndian.Uint32(wav[28:32]); got != 16000 {
		t.Errorf("ByteRate = %d, want 16000", got)
	}
	// BlockAlign = NumChannels * BitsPerSample/8 = 2.
	if got := binary.LittleEndian.Uint16(wav[32:34]); got != 2 {
		t.Errorf("BlockAlign = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Errorf("BitsPerSample = %d, want 16", got)
	}
	if got := string(wav[36:40]); got != "data" {
		t.Errorf("data ID = %q", got)
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != 2000 {
		t.Errorf("Subchunk2Size = %d, want 2000", got)
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
