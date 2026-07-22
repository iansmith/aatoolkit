package driver

import (
	"bytes"
	"encoding/binary"
)

// wavData and buildWAV16kMono are minimal WAV helpers retained purely for tests.
// They formerly backed the local mic-capture path (removed with its own VAD); the
// only remaining user is TestPadWAVSilence, which builds a known WAV and inspects
// the padded result's data chunk. Kept in a _test.go file so they are not shipped
// as unused production code.

// buildWAV16kMono wraps raw 16 kHz mono s16le PCM in a WAV container.
func buildWAV16kMono(pcm []byte) []byte {
	const sampleRate = 16000
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// wavData returns the bytes of a WAV's "data" chunk, scanning chunks (so a
// leading LIST/JUNK chunk is skipped) and tolerating a truncated final chunk.
func wavData(wav []byte) ([]byte, bool) {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, false
	}
	for i := 12; i+8 <= len(wav); {
		id := string(wav[i : i+4])
		sz := int(binary.LittleEndian.Uint32(wav[i+4 : i+8]))
		body := i + 8
		if id == "data" {
			end := min(body+sz, len(wav)) // tolerate a truncated capture
			return wav[body:end], true
		}
		i = body + sz + (sz & 1) // chunks are word-aligned
	}
	return nil, false
}
