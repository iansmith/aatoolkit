package telephony

import "time"

// MuLawFrameMS is the duration of a single Twilio μ-law audio frame.
// Twilio sends 160-sample μ-law frames at 8 kHz = 20 ms per frame.
const MuLawFrameMS = 20

// SampleRateHz is the sample rate of μ-law audio from Twilio.
const SampleRateHz = 8000

// MuLawDuration is how long n bytes of μ-law audio take to play out: one byte
// per sample at SampleRateHz. It is the one definition of this conversion --
// callers derive playout pacing, recording gap lengths and elapsed-audio
// figures from it rather than restating the arithmetic.
//
// The result is exact, keeping the sub-millisecond remainder: 100 bytes is
// 12.5ms, not 12ms. Truncating to whole milliseconds first is a different
// function that agrees only when n is a multiple of 8, so it must not be
// substituted here.
func MuLawDuration(n int) time.Duration {
	return time.Duration(n) * time.Second / SampleRateHz
}

// FrameMS derives the frame duration in milliseconds from the static
// sample rate and the standard Twilio frame size. It is computed once
// at init to allow future flexibility if frame size changes.
var FrameMS = MuLawFrameMS

// MarkEchoGraceMS is the margin, in milliseconds, a mark-echo wait adds atop
// the playout still queued ahead of the mark, to give Twilio a chance to echo
// the mark back. Untyped, so a caller scales it by time.Millisecond.
const MarkEchoGraceMS = 500
