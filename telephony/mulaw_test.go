package telephony

import (
	"bytes"
	"encoding/binary"
	"os/exec"
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

// absErr returns the absolute error between a sample and the decoded value of a μ-law byte.
func absErr(sample int16, b byte) int32 {
	decoded := muLawToLinear(b)
	err := int32(sample) - int32(decoded)
	if err < 0 {
		err = -err
	}
	return err
}

// mag returns the magnitude (absolute value) of the decoded μ-law byte.
func mag(b byte) int32 {
	decoded := muLawToLinear(b)
	if decoded < 0 {
		return int32(-decoded)
	}
	return int32(decoded)
}

// TestLinearToMuLaw_TieBreak asserts that among codes achieving the minimum error,
// the chosen code's decoded magnitude is maximum; and when more than one code ties
// on both error and magnitude (only possible when magnitude is 0), the chosen code is 0xFF.
func TestLinearToMuLaw_TieBreak(t *testing.T) {
	// These samples have multiple codes achieving the same minimum error.
	// For all of them, the chosen code should have the maximum magnitude among
	// the candidates. For sample 0 specifically, the ±0 tie (0xFF and 0x7F both
	// decode to 0) breaks to 0xFF.
	testCases := []struct {
		sample int16
		want   byte
	}{
		{0, 0xFF},  // ±0 tie, both decode to 0, choose 0xFF
		{-1, 0xFF}, // ±0 tie, both decode to 0, choose 0xFF
		{-2, 0xFF}, // ±0 tie, both decode to 0, choose 0xFF
		{-3, 0xFF}, // ±0 tie, both decode to 0, choose 0xFF
		{1, 0xFF},  // ±0 tie, both decode to 0, choose 0xFF
		{2, 0xFF},  // ±0 tie, both decode to 0, choose 0xFF
		{3, 0xFF},  // ±0 tie, both decode to 0, choose 0xFF
		{4, 0xFE},  // three-way tie, step 1 (magnitude) fires: 0xFE > 0xFF = 0x7F
	}

	for _, tc := range testCases {
		got := LinearToMuLaw(tc.sample)
		if got != tc.want {
			t.Errorf("LinearToMuLaw(%d) = 0x%02x, want 0x%02x", tc.sample, got, tc.want)
		}
	}

	// Verify the tie-break property for all 65,536 int16 inputs:
	// among codes achieving minimum error, the chosen code has maximum magnitude.
	// When magnitude ties at zero, prefer 0xFF.
	for i := -32768; i <= 32767; i++ {
		sample := int16(i)
		chosen := LinearToMuLaw(sample)
		chosenErr := absErr(sample, chosen)
		chosenMag := mag(chosen)

		// Find all codes with the same minimum error
		var tiedCodes []byte
		for b := byte(0); b <= 255; b++ {
			if absErr(sample, b) == chosenErr {
				tiedCodes = append(tiedCodes, b)
			}
		}

		// Verify the chosen code has maximum magnitude among tied codes
		for _, b := range tiedCodes {
			bMag := mag(b)
			if bMag > chosenMag {
				t.Errorf("LinearToMuLaw(%d) chose 0x%02x (mag %d), but 0x%02x has mag %d",
					sample, chosen, chosenMag, b, bMag)
				return
			}
		}

		// When magnitude is zero (±0 tie), verify we chose 0xFF
		if chosenMag == 0 {
			if chosen != 0xFF {
				t.Errorf("LinearToMuLaw(%d): when mag=0, chose 0x%02x, want 0xFF",
					sample, chosen)
				return
			}
		}
	}
}

// TestLinearToMuLaw_NeverEmitsNegativeZero asserts that 0x7F is never returned
// for any of the 65,536 int16 inputs. The old encoder returned 0x7F for 8 inputs
// (−3 to +4); this test ensures those are now 0xFF (or 0xFE for sample 4).
func TestLinearToMuLaw_NeverEmitsNegativeZero(t *testing.T) {
	count := 0
	for i := -32768; i <= 32767; i++ {
		sample := int16(i)
		got := LinearToMuLaw(sample)
		if got == 0x7F {
			t.Logf("LinearToMuLaw(%d) = 0x7F (should not occur)", sample)
			count++
		}
	}
	if count != 0 {
		t.Errorf("LinearToMuLaw returned 0x7F for %d inputs; want 0", count)
	}
}

// TestEncodeMuLawFrames_SilenceIsUniformlyFF asserts that an all-zero WAV
// produces frames whose every byte is 0xFF, both real samples and padding.
// This is the regression test for the original defect where samples encoded
// to 0x7F and padding to 0xFF, putting two different "silence" bytes in one frame.
func TestEncodeMuLawFrames_SilenceIsUniformlyFF(t *testing.T) {
	frameSize := SampleRateHz * MuLawFrameMS / 1000
	// frameSize + frameSize/2 samples = 240, yields 2 frames (160 bytes each)
	totalSamples := frameSize + frameSize/2
	wav := pcm16ToWAV(make([]int16, totalSamples), SampleRateHz)

	frames, err := EncodeMuLawFrames(wav)
	if err != nil {
		t.Fatalf("EncodeMuLawFrames: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("want 2 frames, got %d", len(frames))
	}

	// Both frames should be uniformly 0xFF (real samples + padding)
	for frameIdx, frame := range frames {
		for byteIdx, b := range frame {
			if b != 0xFF {
				t.Errorf("frame %d byte %d: want 0xFF, got 0x%02x", frameIdx, byteIdx, b)
			}
		}
	}
}

// TestLinearToMuLaw_IsExactMinimum is a guard test: it asserts that LinearToMuLaw
// returns the code whose decoded value is nearest to the input, for all 65,536 inputs.
// This test passes both before and after the tie-break change; it is the point.
func TestLinearToMuLaw_IsExactMinimum(t *testing.T) {
	for i := -32768; i <= 32767; i++ {
		sample := int16(i)
		got := LinearToMuLaw(sample)
		gotErr := absErr(sample, got)

		// Check that no other byte has strictly lower error
		for b := byte(0); b <= 255; b++ {
			candErr := absErr(sample, b)
			if candErr < gotErr {
				t.Errorf("LinearToMuLaw(%d) = 0x%02x (err %d), but 0x%02x has err %d",
					sample, got, gotErr, b, candErr)
				return
			}
		}
	}
}

// TestLinearToMuLaw_NeverWorseThanFfmpeg is a guard test: it asserts that
// LinearToMuLaw is never strictly worse than ffmpeg's G.711 encoder,
// across all 65,536 int16 inputs. Bit-equality is forbidden (charter R3).
func TestLinearToMuLaw_NeverWorseThanFfmpeg(t *testing.T) {
	// Skip if ffmpeg is not available
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found")
	}

	// Create a test WAV with all 65536 int16 values at s16le, 8kHz, 1 channel
	pcm := make([]int16, 65536)
	for i := 0; i < 65536; i++ {
		pcm[i] = int16(i - 32768)
	}
	wav := pcm16ToWAV(pcm, 8000)

	// Encode with ffmpeg
	cmd := exec.Command("ffmpeg", "-f", "s16le", "-ar", "8000", "-ac", "1",
		"-i", "pipe:0", "-acodec", "pcm_mulaw", "-f", "mulaw", "pipe:1")
	cmd.Stdin = bytes.NewReader(wav[44:]) // Skip WAV header, ffmpeg expects raw PCM
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("ffmpeg encoding failed: %v", err)
	}

	if len(out) != 65536 {
		t.Fatalf("ffmpeg output: want 65536 bytes, got %d", len(out))
	}

	// Compare: our encoder should be >= ffmpeg on all inputs
	worse := 0
	for i := 0; i < 65536; i++ {
		sample := pcm[i]
		ourCode := LinearToMuLaw(sample)
		ffmpegCode := out[i]

		ourErr := absErr(sample, ourCode)
		ffmpegErr := absErr(sample, ffmpegCode)

		if ourErr > ffmpegErr {
			if worse < 10 {
				t.Logf("sample %d: our error %d, ffmpeg error %d", sample, ourErr, ffmpegErr)
			}
			worse++
		}
	}
	if worse > 0 {
		t.Errorf("LinearToMuLaw was strictly worse than ffmpeg on %d inputs", worse)
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
