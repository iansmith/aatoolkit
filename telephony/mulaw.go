package telephony

import (
	"encoding/binary"
	"fmt"
)

// EncodeMuLawFrames encodes a WAV file to G.711 μ-law frames suitable for Twilio.
// It decodes the WAV header, extracts PCM samples, encodes them to μ-law, and
// packs them into 20 ms frames (160 bytes each at 8 kHz), returned as a slice of
// frames rather than one flat blob so a caller can send them one at a time. The
// last frame is zero-padded with 0xFF (the μ-law silence code) if necessary. A
// malformed or unsupported WAV returns a non-nil error.
func EncodeMuLawFrames(wav []byte) ([][]byte, error) {
	pcm, sampleRate, err := decodeWAVToPCM16(wav)
	if err != nil {
		return nil, fmt.Errorf("decode WAV: %w", err)
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
	if numFrames == 0 {
		return nil, nil
	}

	padded := make([]byte, numFrames*frameSize)
	copy(padded, mulaw)
	for i := len(mulaw); i < len(padded); i++ {
		padded[i] = 0xFF
	}

	frames := make([][]byte, numFrames)
	for i := range frames {
		frames[i] = padded[i*frameSize : (i+1)*frameSize]
	}
	return frames, nil
}

// WalkWAVChunks walks a WAV file's RIFF sub-chunks, starting immediately after
// the 12-byte "RIFF"+size+"WAVE" header, and invokes visit for each chunk found
// (its 4-byte id, the offset of its body, and its declared size). Chunk bodies
// are word-aligned per the RIFF spec — an odd-sized chunk is followed by one pad
// byte before the next chunk header — which this walk accounts for. Iteration
// stops as soon as visit returns false, or the chunk headers run past the end of
// wav. Shared by decodeWAVToPCM16 (telephony) and padWAVSilence (driver) so the
// chunk-walk exists in exactly one place.
func WalkWAVChunks(wav []byte, visit func(id string, bodyOffset, size int) (keepGoing bool)) {
	for i := 12; i+8 <= len(wav); {
		id := string(wav[i : i+4])
		sz := int(binary.LittleEndian.Uint32(wav[i+4 : i+8]))
		body := i + 8
		if !visit(id, body, sz) {
			return
		}
		i = body + sz + (sz & 1)
	}
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

	// Walk chunks to find "data" — some WAV writers insert chunks (e.g. "fact",
	// "LIST") between "fmt " and "data", so the data chunk cannot be assumed to
	// immediately follow fmt.
	dataOffset, dataSize := -1, 0
	WalkWAVChunks(wav, func(id string, body, sz int) bool {
		if id == "data" {
			dataOffset, dataSize = body, sz
			return false
		}
		return true
	})
	if dataOffset < 0 {
		return nil, 0, fmt.Errorf("data chunk not found")
	}

	pcmEnd := dataOffset + dataSize
	if pcmEnd > len(wav) {
		return nil, 0, fmt.Errorf("WAV truncated")
	}

	pcmData := wav[dataOffset:pcmEnd]
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

	for b := 0; b <= 255; b++ {
		decoded := muLawToLinear(byte(b))
		err := int32(sample) - int32(decoded)
		if err < 0 {
			err = -err
		}
		if err < minErr {
			minErr = err
			bestByte = byte(b)
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
