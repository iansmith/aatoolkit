package telephony

import (
	"encoding/binary"
	"fmt"
)

// EncodeMuLawFrames encodes a WAV file to G.711 μ-law frames suitable for Twilio.
// TODO: Implement PCM to μ-law encoding with proper G.711 support.
func EncodeMuLawFrames(wav []byte) []byte {
	// Stub: not yet implemented
	return nil
}

// decodeWAVToPCM16 extracts 16-bit PCM samples from a WAV container.
func decodeWAVToPCM16(wav []byte) ([]int16, int, error) {
	if len(wav) < 44 {
		return nil, 0, fmt.Errorf("WAV too short: %d bytes", len(wav))
	}

	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("invalid RIFF/WAVE signature")
	}

	fmtOffset := 12
	if string(wav[fmtOffset:fmtOffset+4]) != "fmt " {
		return nil, 0, fmt.Errorf("fmt chunk not at expected offset")
	}

	sampleRate := int(binary.LittleEndian.Uint32(wav[24:28]))
	bitsPerSample := int(binary.LittleEndian.Uint16(wav[34:36]))
	numChannels := int(binary.LittleEndian.Uint16(wav[22:24]))

	if bitsPerSample != 16 {
		return nil, 0, fmt.Errorf("only 16-bit PCM supported, got %d", bitsPerSample)
	}

	dataOffset := fmtOffset + 8 + int(binary.LittleEndian.Uint32(wav[fmtOffset+4:fmtOffset+8]))
	if dataOffset+8 > len(wav) {
		return nil, 0, fmt.Errorf("data chunk not found")
	}

	if string(wav[dataOffset:dataOffset+4]) != "data" {
		return nil, 0, fmt.Errorf("data chunk signature mismatch")
	}

	dataSize := int(binary.LittleEndian.Uint32(wav[dataOffset+4 : dataOffset+8]))
	pcmStart := dataOffset + 8
	pcmEnd := pcmStart + dataSize

	if pcmEnd > len(wav) {
		return nil, 0, fmt.Errorf("WAV truncated")
	}

	pcmData := wav[pcmStart:pcmEnd]
	numSamples := dataSize / (bitsPerSample / 8) / numChannels
	pcm := make([]int16, numSamples)

	for i := 0; i < numSamples; i++ {
		sampleOffset := i * (bitsPerSample / 8) * numChannels
		pcm[i] = int16(binary.LittleEndian.Uint16(pcmData[sampleOffset : sampleOffset+2]))
	}

	return pcm, sampleRate, nil
}

// linearToMuLaw encodes a 16-bit PCM sample to a G.711 μ-law byte.
// TODO: Implement proper G.711 μ-law encoding.
func linearToMuLaw(sample int16) byte {
	return 0xFF
}

// resampleToMono8kHz resamples to mono 8 kHz.
func resampleToMono8kHz(pcm []int16, sourceSampleRate int) []int16 {
	return pcm
}
