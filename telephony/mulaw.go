package telephony

import (
	"encoding/binary"
	"fmt"
)

// EncodeMuLawFrames encodes a WAV file to G.711 μ-law frames suitable for Twilio.
// It decodes the WAV header, extracts PCM samples, encodes them to μ-law, and
// packs them into 20 ms frames (160 bytes each at 8 kHz). The last frame is
// zero-padded with 0xFF (the μ-law silence code) if necessary.
func EncodeMuLawFrames(wav []byte) []byte {
	pcm, sampleRate, err := decodeWAVToPCM16(wav)
	if err != nil {
		return nil
	}

	if sampleRate != 8000 {
		pcm = resampleToMono8kHz(pcm, sampleRate)
	}

	mulaw := make([]byte, len(pcm))
	for i, sample := range pcm {
		mulaw[i] = linearToMuLaw(sample)
	}

	frameSize := SampleRateHz * MuLawFrameMS / 1000
	numFrames := (len(mulaw) + frameSize - 1) / frameSize
	frames := make([]byte, numFrames*frameSize)

	copy(frames, mulaw)
	for i := len(mulaw); i < len(frames); i++ {
		frames[i] = 0xFF
	}

	return frames
}

// decodeWAVToPCM16 extracts 16-bit PCM samples from a WAV container.
func decodeWAVToPCM16(wav []byte) ([]int16, int, error) {
	if len(wav) < 44 {
		return nil, 0, fmt.Errorf("WAV too short")
	}

	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("invalid WAV")
	}

	fmtOffset := 12
	if string(wav[fmtOffset:fmtOffset+4]) != "fmt " {
		return nil, 0, fmt.Errorf("fmt chunk error")
	}

	sampleRate := int(binary.LittleEndian.Uint32(wav[24:28]))
	bitsPerSample := int(binary.LittleEndian.Uint16(wav[34:36]))
	numChannels := int(binary.LittleEndian.Uint16(wav[22:24]))

	if bitsPerSample != 16 {
		return nil, 0, fmt.Errorf("only 16-bit PCM supported")
	}
	if numChannels < 1 {
		return nil, 0, fmt.Errorf("invalid channel count")
	}

	// Walk chunks from fmtOffset to find "data" — some WAV writers insert
	// chunks (e.g. "fact", "LIST") between "fmt " and "data", so the data
	// chunk cannot be assumed to immediately follow fmt.
	dataOffset := -1
	dataSize := 0
	for i := fmtOffset; i+8 <= len(wav); {
		id := string(wav[i : i+4])
		sz := int(binary.LittleEndian.Uint32(wav[i+4 : i+8]))
		body := i + 8
		if id == "data" {
			dataOffset, dataSize = i, sz
			break
		}
		i = body + sz + (sz & 1)
	}
	if dataOffset < 0 {
		return nil, 0, fmt.Errorf("data chunk not found")
	}

	pcmStart := dataOffset + 8
	pcmEnd := pcmStart + dataSize

	if pcmEnd > len(wav) {
		return nil, 0, fmt.Errorf("WAV truncated")
	}

	pcmData := wav[pcmStart:pcmEnd]
	bytesPerSample := bitsPerSample / 8
	numFrames := dataSize / bytesPerSample / numChannels
	pcm := make([]int16, numFrames)

	for i := 0; i < numFrames; i++ {
		frameOffset := i * bytesPerSample * numChannels
		var sum int32
		for c := 0; c < numChannels; c++ {
			sampleOffset := frameOffset + c*bytesPerSample
			sum += int32(int16(binary.LittleEndian.Uint16(pcmData[sampleOffset : sampleOffset+2])))
		}
		pcm[i] = int16(sum / int32(numChannels))
	}

	return pcm, sampleRate, nil
}

// linearToMuLaw encodes a 16-bit PCM sample to a G.711 μ-law byte.
// Finds the μ-law byte that best represents the input by searching all 256 values.
func linearToMuLaw(sample int16) byte {
	minErr := int32(32768)
	bestByte := byte(0xFF)

	for b := byte(0); b < 255; b++ {
		decoded := muLawToLinear(b)
		err := int32(sample) - int32(decoded)
		if err < 0 {
			err = -err
		}
		if err < minErr {
			minErr = err
			bestByte = b
		}
	}

	return bestByte
}

// resampleToMono8kHz resamples to mono 8 kHz using linear interpolation.
func resampleToMono8kHz(pcm []int16, sourceSampleRate int) []int16 {
	if sourceSampleRate == 8000 {
		return pcm
	}

	targetCount := len(pcm) * 8000 / sourceSampleRate
	result := make([]int16, targetCount)

	for i := 0; i < targetCount; i++ {
		srcIdx := float64(i) * float64(sourceSampleRate) / 8000.0
		srcIdxInt := int(srcIdx)
		frac := srcIdx - float64(srcIdxInt)

		if srcIdxInt+1 < len(pcm) {
			s1 := float64(pcm[srcIdxInt])
			s2 := float64(pcm[srcIdxInt+1])
			result[i] = int16(s1 + (s2-s1)*frac)
		} else if srcIdxInt < len(pcm) {
			result[i] = pcm[srcIdxInt]
		}
	}

	return result
}
