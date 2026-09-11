package main

import (
	"bytes"
	"testing"

	"github.com/iansmith/aatoolkit/telephony"
)

// The scale's length is the measurement's range: an open longer than the scale
// eats all of it and reports only "at least this long". 3.0s covers the ~2.5s
// measured on 2026-09-11 with headroom.
func TestWarmTone_IsThreeSecondsOfWholeFrames(t *testing.T) {
	got := warmToneFrames()

	const want = 6 * 500 * telephony.SampleRateHz / 1000 // 6 tones x 500ms @ 8kHz
	if len(got) != want {
		t.Errorf("scale is %d bytes (%.2fs), want %d (%.2fs)",
			len(got), float64(len(got))/telephony.SampleRateHz, want, float64(want)/telephony.SampleRateHz)
	}
	// dial feeds this frame by frame so pacing governs it. A length that is not
	// a whole number of frames makes the final frame short, which puts every
	// subsequent frame off the 20ms grid.
	if len(got)%muLawFrame20ms != 0 {
		t.Errorf("scale is %d bytes, not a whole number of %d-byte frames; the last frame would be short and the stream would leave the 20ms grid",
			len(got), muLawFrame20ms)
	}
	if n := len(warmToneScale); n != 6 {
		t.Errorf("scale has %d tones, want 6 — the count and the 500ms step together are what make a missing prefix readable as a duration", n)
	}
}

// Rising, and by a semitone each time. A scale the ear cannot order gives no
// answer to "which tone did it start on", which is the whole measurement.
func TestWarmTone_RisesBySemitones(t *testing.T) {
	const semitone = 1.059463 // 2^(1/12)
	for i := 1; i < len(warmToneScale); i++ {
		ratio := warmToneScale[i] / warmToneScale[i-1]
		if ratio < semitone-0.001 || ratio > semitone+0.001 {
			t.Errorf("tone %d/%d ratio = %.6f (%.2f -> %.2f Hz), want %.6f — equal temperament, A4=440",
				i, i-1, ratio, warmToneScale[i-1], warmToneScale[i], semitone)
		}
	}
	// Every tone must stay under the Nyquist limit of 8kHz mu-law or it aliases
	// and the operator hears a frequency that is not in the scale.
	for i, hz := range warmToneScale {
		if hz >= telephony.SampleRateHz/2 {
			t.Errorf("tone %d is %.2f Hz, at or above the %d Hz Nyquist limit — it would alias",
				i, hz, telephony.SampleRateHz/2)
		}
	}
}

// Each tone fades in and out. A hard cut is a step discontinuity, which is a
// click, and six clicks is a worse diagnostic than no scale.
func TestWarmTone_TonesFadeRatherThanClick(t *testing.T) {
	got := warmToneFrames()
	const silence = telephony.MuLawSilence

	// The first sample of the scale and the first sample of each later tone are
	// at the start of a fade, so all of them must be at or very near silence.
	for i := range warmToneScale {
		at := i * warmToneStep
		if d := muLawDistance(got[at], silence); d > 2 {
			t.Errorf("tone %d starts at mu-law %#x, %d codes from silence (%#x); a tone that starts at amplitude is a click",
				i, got[at], d, silence)
		}
		last := at + warmToneStep - 1
		if d := muLawDistance(got[last], silence); d > 2 {
			t.Errorf("tone %d ends at mu-law %#x, %d codes from silence (%#x); a tone cut mid-cycle is a click",
				i, got[last], d, silence)
		}
	}
}

func muLawDistance(a, b byte) int {
	d := int(a) - int(b)
	if d < 0 {
		return -d
	}
	return d
}

// The scale is written into the sink at OPEN, before the writer goroutine
// exists -- not handed to play().
//
// That is the whole design. Through play() it would be queued audio, and
// telling the filler about it advances fedThrough, which outstanding() also
// reads: the client would believe the SERVER was speaking for three seconds,
// holding the mic gate shut and pushing every mark echo past the engine's 1s
// bound. Five tests failed exactly that way before the scale moved here.
func TestWarmTone_PrimeIsWrittenAtOpenAheadOfEverything(t *testing.T) {
	sink := &recordingSink{}
	prime := warmToneFrames()
	p := newAudioPlayer(sink, nil, prime)

	// Already in the sink before a single frame has been played.
	got, writes := sink.snapshot()
	if len(got) != len(prime) {
		t.Fatalf("sink holds %d bytes before any play(), want the %d-byte scale written at open",
			len(got), len(prime))
	}
	if writes != 1 {
		t.Errorf("scale reached the sink in %d writes, want 1 at open", writes)
	}
	if !bytes.Equal(got, prime) {
		t.Error("the bytes at open are not the scale")
	}

	// And call audio lands behind it, not interleaved with it.
	if err := p.play(mkFrame(0x2A)); err != nil {
		t.Fatalf("play: %v", err)
	}
	p.settle()
	after, _ := sink.snapshot()
	if !bytes.Equal(after[:len(prime)], prime) {
		t.Error("call audio disturbed the scale; the scale must sit ahead of everything the call produces")
	}
	_ = p.close()
}

// nil prime writes nothing -- the off state must cost exactly zero.
func TestWarmTone_NoPrimeWritesNothing(t *testing.T) {
	sink := &recordingSink{}
	p := newAudioPlayer(sink, nil, nil)
	if got, writes := sink.snapshot(); len(got) != 0 || writes != 0 {
		t.Errorf("a nil prime wrote %d bytes in %d writes, want none", len(got), writes)
	}
	_ = p.close()
}

// The scale is opt-in. It puts 3s in ffplay's pipe ahead of the call, so every
// call's audio is 3s late and the pipe stays that deep -- which is the very
// depth the paced writer exists to keep shallow, and which a barge-in flush
// cannot reach. Worth it on a diagnostic call, not on every call.
func TestWarmTone_PrimeIsOffByDefault(t *testing.T) {
	if warmTonePrime != nil {
		t.Fatalf("warmTonePrime is %d bytes by default; every call would run 3s behind with a pipe the flush cannot reach",
			len(warmTonePrime))
	}
}

// -warm-tones must actually arm the scale. A parsed flag that is never applied
// is inert, and an inert flag is indistinguishable from a working one in every
// other test here -- which is how a mutation removing the wiring survived.
func TestWarmTone_FlagArmsAndDisarmsThePrime(t *testing.T) {
	t.Cleanup(func() { setWarmTones(false) })

	setWarmTones(true)
	if len(warmTonePrime) != len(warmToneFrames()) {
		t.Errorf("setWarmTones(true) left the prime at %d bytes, want the %d-byte scale; -warm-tones would be silently inert",
			len(warmTonePrime), len(warmToneFrames()))
	}
	setWarmTones(false)
	if warmTonePrime != nil {
		t.Errorf("setWarmTones(false) left %d bytes armed; the scale would keep playing after it was turned off", len(warmTonePrime))
	}
}
