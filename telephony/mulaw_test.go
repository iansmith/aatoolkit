package telephony

import (
	"encoding/binary"
	"testing"
)

// TestEncodeMuLawFrames_FrameSizeAndPad asserts that EncodeMuLawFrames produces
// frames of exactly SampleRateHz*MuLawFrameMS/1000 bytes each, across multiple
// frames (not just a single one), with only the last frame zero-padded to 0xFF.
func TestEncodeMuLawFrames_FrameSizeAndPad(t *testing.T) {
	frameSize := SampleRateHz * MuLawFrameMS / 1000
	// Two and a half frames' worth of samples, so the last frame needs padding
	// and there are multiple full frames to check.
	totalSamples := frameSize*2 + frameSize/2
	wav := pcm16ToWAV(make([]int16, totalSamples), SampleRateHz)

	frames, err := EncodeMuLawFrames(wav)
	if err != nil {
		t.Fatalf("EncodeMuLawFrames: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("want 3 frames, got %d", len(frames))
	}

	for i, frame := range frames {
		if len(frame) != frameSize {
			t.Errorf("frame %d: want %d bytes, got %d", i, frameSize, len(frame))
		}
	}

	last := frames[2]
	for i := frameSize / 2; i < frameSize; i++ {
		if last[i] != 0xFF {
			t.Errorf("pad byte at frame 2 offset %d: want 0xFF, got 0x%02x", i, last[i])
		}
	}
}

// TestEncodeMuLawFrames_PropagatesDecodeError asserts a malformed WAV surfaces
// an error instead of silently returning nil/empty frames.
func TestEncodeMuLawFrames_PropagatesDecodeError(t *testing.T) {
	_, err := EncodeMuLawFrames([]byte("not a wav"))
	if err == nil {
		t.Fatal("want a non-nil error for a malformed WAV, got nil")
	}
}

// referenceLinearToMuLaw independently re-implements the same nearest-code
// search as linearToMuLaw, in this test file rather than calling production's
// function, so a bug in production's search bounds (e.g. an off-by-one that
// silently excludes a candidate byte) shows up as a mismatch here instead of a
// false pass. It is the in-test oracle the ticket calls for — computed from the
// existing muLawToLinear decode table, not hand-typed.
func referenceLinearToMuLaw(sample int16) byte {
	best := byte(0)
	bestErr := int32(1<<31 - 1)
	for b := 0; b <= 255; b++ {
		decoded := muLawToLinear(byte(b))
		err := int32(sample) - int32(decoded)
		if err < 0 {
			err = -err
		}
		if err < bestErr {
			bestErr = err
			best = byte(b)
		}
	}
	return best
}

// TestMuLaw_RoundTrip encodes a PCM ramp and asserts every single encoded byte
// exactly matches the independently-computed G.711 oracle above — not a loose
// statistical tolerance, since the oracle removes the need for one.
func TestMuLaw_RoundTrip(t *testing.T) {
	const numSamples = 8000
	pcm := make([]int16, numSamples)
	for i := range pcm {
		pcm[i] = int16((int64(i) * 65536 / int64(numSamples)) - 32768)
	}

	wav := pcm16ToWAV(pcm, SampleRateHz)
	frames, err := EncodeMuLawFrames(wav)
	if err != nil {
		t.Fatalf("EncodeMuLawFrames: %v", err)
	}
	frameSize := SampleRateHz * MuLawFrameMS / 1000
	flat := make([]byte, 0, len(frames)*frameSize)
	for _, f := range frames {
		flat = append(flat, f...)
	}

	for i, sample := range pcm {
		want := referenceLinearToMuLaw(sample)
		got := flat[i]
		if got != want {
			t.Fatalf("sample %d (%d): encoded 0x%02x, want 0x%02x (oracle)", i, sample, got, want)
		}
	}

	// Round-trip sanity: decoding the encoded frames reproduces the same
	// samples muLawToLinear(want) would, for every sample — this is exact
	// (not a tolerance), since flat[i] was just proven to equal want above.
	decoded := decodeMuLaw(flat[:len(pcm)])
	for i, sample := range pcm {
		wantDecoded := muLawToLinear(referenceLinearToMuLaw(sample))
		gotDecoded := int16(decoded[i] * 32768)
		if gotDecoded != wantDecoded {
			t.Errorf("sample %d: decodeMuLaw reconstructed %d, want %d", i, gotDecoded, wantDecoded)
		}
	}
}

func pcm16ToWAV(pcm []int16, sampleRate int) []byte {
	const (
		pcmBytesPerSample = 2
		numChannels       = 1
		bitsPerSample     = 16
	)
	pcmLen := len(pcm) * pcmBytesPerSample
	wav := make([]byte, 44+pcmLen)

	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+pcmLen))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], numChannels)
	binary.LittleEndian.PutUint32(wav[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(wav[28:32], uint32(sampleRate*numChannels*pcmBytesPerSample))
	binary.LittleEndian.PutUint16(wav[32:34], numChannels*pcmBytesPerSample)
	binary.LittleEndian.PutUint16(wav[34:36], bitsPerSample)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(pcmLen))

	for i, sample := range pcm {
		off := 44 + i*pcmBytesPerSample
		binary.LittleEndian.PutUint16(wav[off:off+2], uint16(sample))
	}
	return wav
}
