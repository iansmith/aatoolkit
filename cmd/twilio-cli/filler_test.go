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

	// Driven the way the read loop drives it: one call per 20ms tick. A single
	// call jumping the whole gap is not a shape the loop produces -- it only
	// happens when the loop itself stalled, which is what maxFillCatchUp
	// covers (see the test below).
	var written int
	frames := 0
	step := telephony.MuLawDuration(muLawFrame20ms)
	for elapsed := step; elapsed <= 47*time.Second; elapsed += step {
		frames += f.fill(base.Add(elapsed), func(b []byte) { written += len(b) })
	}

	wantFrames := int(47 * time.Second / step)
	if frames != wantFrames {
		t.Errorf("frames written across a 47s gap: got %d, want %d", frames, wantFrames)
	}
	if got := telephony.MuLawDuration(written); got != 47*time.Second {
		t.Errorf("silence written: %s, want 47s", got)
	}
}

// TestPlayoutFiller_ResyncsRatherThanBlockingAfterAStall bounds the catch-up.
//
// The loop ticks every 20ms, so a large deficit means the loop itself stopped
// running -- a machine that slept, or a write that blocked. Filling that
// honestly means pushing minutes of silence into ffplay's 64KB stdin pipe in
// one loop; ffplay drains at 8000 B/s, so the write blocks and takes the only
// websocket reader with it. That turns a stall into a hang, which is worse
// than the gap being covered.
//
// A skip loses continuity for a moment. A hang loses the call.
func TestPlayoutFiller_ResyncsRatherThanBlockingAfterAStall(t *testing.T) {
	f := newTestFiller()

	frames := 0
	f.fill(base.Add(5*time.Minute), func([]byte) { frames++ })
	if frames != 0 {
		t.Errorf("wrote %d frames to cover a 5 minute stall; want a resync, not a catch-up", frames)
	}

	// And it must be usable immediately afterwards, not left behind again.
	resumed := base.Add(5 * time.Minute)
	if n := f.fill(resumed.Add(100*time.Millisecond), func([]byte) {}); n != 5 {
		t.Errorf("after resync, 100ms of gap wrote %d frames, want 5", n)
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
		now = now.Add(telephony.MuLawDuration(muLawFrame20ms))
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

// TestPlayoutFiller_OutstandingIsWhatHasNotPlayedYet pins the quantity a mark
// echo must wait for, and the case the old arithmetic got wrong.
//
// markEchoDelay computed playout(bytesSinceMark) - elapsed, where elapsed ran
// from the PREVIOUS mark. That makes elapsed include dead air that arrived
// before the audio did, so on the shape this whole branch exists to handle --
// a mark, then a long silence, then a burst faster than real time -- it
// returned zero and the mark was echoed while seconds were still queued. The
// server reads a mark echo as playout-complete, so it advanced the turn before
// the caller had heard the reply. Late by the send duration was the old bug;
// early by the whole burst is worse, because late costs a timeout and early
// cuts the caller off.
//
// outstanding asks the only question that matters: how much audio has been
// handed over that has not played yet.
func TestPlayoutFiller_OutstandingIsWhatHasNotPlayedYet(t *testing.T) {
	oneSecond := make([]byte, telephony.SampleRateHz)

	t.Run("a burst is still queued", func(t *testing.T) {
		f := newTestFiller()
		f.fed(oneSecond, base)
		if got := f.outstanding(base); got != time.Second {
			t.Errorf("outstanding right after a 1s burst = %s, want 1s", got)
		}
		if got := f.outstanding(base.Add(400 * time.Millisecond)); got != 600*time.Millisecond {
			t.Errorf("outstanding 400ms later = %s, want 600ms", got)
		}
	})

	t.Run("idle before the audio does not discount it", func(t *testing.T) {
		// THE CASE THE OLD TABLE HAD NO ENTRY FOR, and the one measured live:
		// 47s of silence, then 4.9s of speech delivered in 2.4s.
		f := newTestFiller()
		gapEnd := base.Add(47 * time.Second)
		f.fill(gapEnd, func([]byte) {}) // the filler covered the silence
		burst := make([]byte, 5*telephony.SampleRateHz)
		f.fed(burst, gapEnd)

		got := f.outstanding(gapEnd)
		if got != 5*time.Second {
			t.Errorf("outstanding = %s, want 5s. The old formula charged the 47s "+
				"of preceding silence against the audio and returned 0, echoing the "+
				"mark while the whole burst was still queued", got)
		}
	})

	t.Run("nothing queued once it has played", func(t *testing.T) {
		f := newTestFiller()
		f.fed(oneSecond, base)
		if got := f.outstanding(base.Add(2 * time.Second)); got != 0 {
			t.Errorf("outstanding = %s, want 0 -- never negative", got)
		}
	})
}

// TestPlayoutFiller_FlushDropsQueuedPlayout pins what a Twilio `clear` does to
// the filler.
//
// `clear` is barge-in: the caller started talking, so the server abandons the
// rest of its reply and tells the client to drop whatever of it is still
// queued. The filler is twilio-cli's model of that queue -- fedThrough is the
// instant through which audio has been handed over -- so honoring the clear
// means fedThrough comes back to now. Two things then follow, and this test
// asserts both because either alone can be got right by accident:
//
//   - Nothing is outstanding, so a mark arriving after the clear echoes at
//     once instead of waiting out audio that was thrown away.
//   - Filling resumes immediately. While fedThrough sat seconds ahead, fill
//     wrote nothing -- correct for audio that is about to play, wrong for
//     audio that was discarded, which leaves the player with no bytes at all
//     across the very stretch the caller is talking over.
func TestPlayoutFiller_FlushDropsQueuedPlayout(t *testing.T) {
	f := newTestFiller()

	// 5 seconds of reply delivered in one burst, the shape the speech backend
	// actually produces.
	f.fed(make([]byte, 5*telephony.SampleRateHz), base)

	// Barge-in half a second in: 4.5s of it has not played.
	clearAt := base.Add(500 * time.Millisecond)
	if got := f.outstanding(clearAt); got != 4500*time.Millisecond {
		t.Fatalf("outstanding before the clear = %s, want 4.5s (test setup is wrong)", got)
	}

	f.flush(clearAt)

	if got := f.outstanding(clearAt); got != 0 {
		t.Errorf("outstanding after a clear = %s, want 0 -- the discarded audio is still "+
			"charged against the next mark echo, so the server waits out a reply nobody hears", got)
	}

	step := telephony.MuLawDuration(muLawFrame20ms)
	if n := f.fill(clearAt.Add(step), func([]byte) {}); n != 1 {
		t.Errorf("frames filled one tick after a clear: got %d, want 1 -- the player is "+
			"being starved for as long as the flushed audio would have run", n)
	}
}
