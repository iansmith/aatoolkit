package telephony

import (
	"encoding/binary"
	"testing"
)

// TestEncodeMuLawFrames_FrameSizeAndPad asserts that EncodeMuLawFrames produces
// frames of exactly SampleRateHz*MuLawFrameMS/1000 bytes, with the last frame
// zero-padded to 0xFF.
func TestEncodeMuLawFrames_FrameSizeAndPad(t *testing.T) {
	halfFrame := SampleRateHz * MuLawFrameMS / 1000 / 2
	wav := pcm16ToWAV(make([]int16, halfFrame), SampleRateHz)

	frames := EncodeMuLawFrames(wav)
	expectedFrameSize := SampleRateHz * MuLawFrameMS / 1000

	if len(frames) != expectedFrameSize {
		t.Fatalf("want %d bytes, got %d", expectedFrameSize, len(frames))
	}

	for i := halfFrame; i < expectedFrameSize; i++ {
		if frames[i] != 0xFF {
			t.Errorf("pad byte at %d: want 0xFF, got 0x%02x", i, frames[i])
		}
	}
}

// TestMuLaw_RoundTrip encodes a PCM signal, decodes, and checks round-trip.
func TestMuLaw_RoundTrip(t *testing.T) {
	const numSamples = 8000
	pcm := make([]int16, numSamples)
	for i := range pcm {
		pcm[i] = int16((int64(i) * 65536 / int64(numSamples)) - 32768)
	}

	wav := pcm16ToWAV(pcm, SampleRateHz)
	frames := EncodeMuLawFrames(wav)
	decoded := decodeMuLaw(frames)

	passCount := 0
	for i, sample := range pcm {
		reconstructed := int16(decoded[i] * 32767)
		err := int32(sample) - int32(reconstructed)
		if err < 0 {
			err = -err
		}
		if err <= 16000 {
			passCount++
		}
	}

	passRatio := float64(passCount) / float64(len(pcm))
	if passRatio < 0.9 {
		t.Errorf("only %.1f%% in range (pass=%d/%d)", passRatio*100, passCount, len(pcm))
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
