package driver

import (
	"bytes"
	"encoding/binary"
	"strings"
	"time"
	"unicode"
)

// endDecision is what the endpointer wants the driver to do after a VAD span.
type endDecision int

const (
	keepRecording    endDecision = iota // keep capturing
	confirmCandidate                    // transcribe the last short clip, then call confirmed()
	endTurn                             // finalize the turn
)

// stage is the endpointer's internal state.
type stage int

const (
	stListening  stage = iota // speaking / brief gaps
	stArmed                   // >= armSilence of trailing silence seen
	stPending                 // a short post-arm clip seen; awaiting isolating silence
	stConfirming              // confirmCandidate emitted; awaiting confirmed()
	stEnded                   // turn is over (latched)
)

// endpointer implements the silence-gated "Done" state machine (design §2-§4):
// LISTENING → ARMED → (short candidate) → CONFIRMING → END. It consumes VAD spans
// (contiguous runs of voiced/unvoiced audio) and decides when the turn ends.
//
//   - ARMED once trailing silence reaches armSilence.
//   - A resumed voiced span <= maxDoneClip, then isolated by silence, is a
//     "Done" candidate → confirmCandidate. A longer resumed span is a
//     think-pause continuation → keep listening (un-arm).
//   - confirmed(true) ends the turn; confirmed(false) resumes and requires a
//     fresh arm before the next candidate.
//   - trailing silence reaching absSilence force-ends the turn (fallback).
//
// It is a pure decision unit: all I/O (ffmpeg, buffer, transcription) lives
// outside it, which keeps it unit-testable.
type endpointer struct {
	armSilence  time.Duration
	absSilence  time.Duration
	maxDoneClip time.Duration

	st       stage
	trailing time.Duration // accumulated trailing silence (reset by any voiced span)
	spoken   bool          // at least one real voiced span seen — so leading silence can't arm
}

func newEndpointer(arm, abs, maxDone time.Duration) *endpointer {
	return &endpointer{armSilence: arm, absSilence: abs, maxDoneClip: maxDone}
}

// onSpan feeds one VAD span: a contiguous run of voiced (or unvoiced) audio of
// duration d.
func (e *endpointer) onSpan(voiced bool, d time.Duration) endDecision {
	if e.st == stEnded {
		return endTurn // latched
	}

	if voiced {
		e.trailing = 0
		if d > 0 {
			e.spoken = true
		}
		switch e.st {
		case stArmed:
			if d <= e.maxDoneClip {
				e.st = stPending // a short clip after the pause — maybe "Done"
			} else {
				e.st = stListening // long continuation — a think-pause, not the end
			}
		default:
			// pending/confirming interrupted by speech, or plain listening
			e.st = stListening
		}
		return keepRecording
	}

	// silence
	e.trailing += d
	if e.trailing >= e.absSilence {
		e.st = stEnded
		return endTurn
	}
	switch e.st {
	case stListening:
		if e.spoken && e.trailing >= e.armSilence {
			e.st = stArmed
		}
	case stPending:
		e.st = stConfirming
		return confirmCandidate
	}
	return keepRecording
}

// confirmed reports whether a confirmCandidate clip transcribed to a stopword.
// Spurious calls (no candidate pending) are a safe no-op.
func (e *endpointer) confirmed(isStopword bool) endDecision {
	if e.st != stConfirming {
		return keepRecording
	}
	if isStopword {
		e.st = stEnded
		return endTurn
	}
	// false alarm: resume, and require a fresh arm before the next candidate
	e.st = stListening
	e.trailing = 0
	return keepRecording
}

// isStopword reports whether a (short, isolated) transcript is exactly one of the
// stopwords, ignoring case, surrounding whitespace, and surrounding punctuation
// on both the input and each list entry.
func isStopword(text string, stopwords []string) bool {
	t := normalizeWord(text)
	if t == "" {
		return false
	}
	for _, w := range stopwords {
		if normalizeWord(w) == t {
			return true
		}
	}
	return false
}

func normalizeWord(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// wavHead returns the WAV truncated to its first d of audio (16 kHz mono s16le) —
// used to cut the turn buffer at the end marker, dropping the "Done" clip and the
// tail after it. Returns nil for a non-WAV input.
func wavHead(wav []byte, d time.Duration) []byte {
	data, ok := wavData(wav)
	if !ok {
		return nil
	}
	const bytesPerSec = 16000 * 2 // 16 kHz, mono, 2 bytes/sample
	keep := int(d.Seconds() * bytesPerSec)
	keep -= keep % 2 // whole samples
	if keep < 0 {
		keep = 0
	}
	if keep > len(data) {
		keep = len(data)
	}
	return buildWAV16kMono(data[:keep])
}

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
