package main

import (
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// base is an arbitrary fixed instant; every case works in offsets from it, so
// nothing here depends on a real clock.
var base = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func newTestFiller() *playoutFiller { return newPlayoutFiller(base, telephony.MuLawSilence) }

// TestPlayoutFiller_SilentGapIsFilledAtRealTime is the whole point: a call
// that goes quiet for 47 seconds must not become a stream that skips 47
// seconds. Measured on a live demo call, that gap is exactly what preceded the
// caller hearing nothing for the rest of the call.
func TestPlayoutFiller_SilentGapIsFilledAtRealTime(t *testing.T) {
	f := newTestFiller()

	var written int
	n := f.fill(base.Add(47*time.Second), func(b []byte) { written += len(b) })

	wantFrames := int(47 * time.Second / mulawPlayoutDuration(muLawFrame20ms))
	if n != wantFrames {
		t.Errorf("frames written for a 47s gap: got %d, want %d", n, wantFrames)
	}
	if got := mulawPlayoutDuration(written); got != 47*time.Second {
		t.Errorf("silence written: %s, want 47s", got)
	}
}

// TestPlayoutFiller_NoSilenceWhileAudioFlows guards the other direction: the
// filler must be inert on a normal paced stream, or it doubles every call's
// audio and puts playback permanently behind.
func TestPlayoutFiller_NoSilenceWhileAudioFlows(t *testing.T) {
	f := newTestFiller()
	frame := make([]byte, muLawFrame20ms)

	now := base
	total := 0
	for i := 0; i < 500; i++ { // 10 seconds of real-time media
		now = now.Add(mulawPlayoutDuration(muLawFrame20ms))
		f.fed(frame, now)
		total += f.fill(now, func([]byte) {})
	}

	if total != 0 {
		t.Errorf("filler wrote %d silence frames into a real-time stream; want 0", total)
	}
}

// TestPlayoutFiller_BurstAheadOfRealTimeIsNotPaddedOver covers the case that
// makes a naive "has anything arrived lately?" check wrong. A server that
// sends an utterance faster than real time -- which is what the speech backend
// does -- is AHEAD, not behind, and padding it would insert silence into the
// middle of a sentence.
func TestPlayoutFiller_BurstAheadOfRealTimeIsNotPaddedOver(t *testing.T) {
	f := newTestFiller()

	// 5 seconds of audio delivered in one burst at t=0.
	burst := make([]byte, 5*telephony.SampleRateHz)
	f.fed(burst, base)

	// Two seconds later the player is still working through it.
	if n := f.fill(base.Add(2*time.Second), func([]byte) {}); n != 0 {
		t.Errorf("filler wrote %d frames while the player still had 3s queued; want 0", n)
	}

	// Only once the burst has played out does filling resume.
	if n := f.fill(base.Add(6*time.Second), func([]byte) {}); n == 0 {
		t.Error("filler wrote nothing a second after the burst finished; the stream is stalled again")
	}
}

// TestPlayoutFiller_ResumesFromNowAfterAGap pins which of the two candidate
// origins fed() advances from.
//
// After a gap the stream has been padded up to roughly now, and the arriving
// audio starts here. Advancing from a stale fedThrough instead would leave the
// filler believing it was behind and pad again on the next tick, injecting
// silence on top of live speech.
func TestPlayoutFiller_ResumesFromNowAfterAGap(t *testing.T) {
	f := newTestFiller()
	oneSecond := make([]byte, telephony.SampleRateHz)

	// A gap with nothing filling it -- a caller could have had the loop parked
	// on a blocking read -- then audio arrives 30 seconds in.
	arrival := base.Add(30 * time.Second)
	f.fed(oneSecond, arrival)

	// Half a second later, half of that second is still queued: no padding.
	if n := f.fill(arrival.Add(500*time.Millisecond), func([]byte) {}); n != 0 {
		t.Errorf("filler wrote %d frames over live audio; want 0", n)
	}
}
