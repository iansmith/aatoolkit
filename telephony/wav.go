package telephony

import (
	"encoding/binary"
)

// wavHeaderSize is the size of the canonical 44-byte RIFF/WAVE header written
// by mulawToWAV: 12-byte RIFF chunk + 24-byte fmt chunk + 8-byte data header.
const wavHeaderSize = 44

// mulawToWAV decodes G.711 μ-law bytes to 16-bit signed PCM and wraps them in a
// mono WAV container at sampleRate. Decoding goes through muLawToLinear (vad.go)
// — the same decoder the Silero VAD path is validated against — so the audio the
// STT sidecar hears is bit-identical to the audio the VAD made its decisions on.
func mulawToWAV(mulaw []byte, sampleRate int) []byte {
	const (
		pcmBytesPerSample = 2
		numChannels       = 1
		bitsPerSample     = 16
	)
	pcmLen := len(mulaw) * pcmBytesPerSample

	wav := make([]byte, wavHeaderSize+pcmLen)

	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+pcmLen))
	copy(wav[8:12], "WAVE")

	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(wav[20:22], 1)  // AudioFormat: 1 = PCM
	binary.LittleEndian.PutUint16(wav[22:24], numChannels)
	binary.LittleEndian.PutUint32(wav[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(wav[28:32], uint32(sampleRate*numChannels*pcmBytesPerSample)) // ByteRate
	binary.LittleEndian.PutUint16(wav[32:34], numChannels*pcmBytesPerSample)                    // BlockAlign
	binary.LittleEndian.PutUint16(wav[34:36], bitsPerSample)

	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(pcmLen))

	for i, b := range mulaw {
		off := wavHeaderSize + i*pcmBytesPerSample
		binary.LittleEndian.PutUint16(wav[off:off+2], uint16(muLawToLinear(b)))
	}

	return wav
}
