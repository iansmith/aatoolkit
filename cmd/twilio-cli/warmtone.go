package main

import (
	"math"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// The device-open problem, and why this file plays tones rather than silence.
//
// ffplay's CoreAudio output device takes time to open -- on a call, contending
// with ffmpeg opening the CAPTURE device at the same moment -- and audio handed
// over during that window is not heard. Measured on the demo call of
// 2026-09-11: roughly 2.5s of the server's opening sentence, lost with the
// played stream byte-identical to the clip and handed over at exactly one
// second per second. The bytes were right, the timing was right, and the
// opening was still gone (AATK-134).
//
// So the remedy is to put something disposable in front of the call's first
// real audio, and let the open window consume that instead.
//
// Silence would do the job and tell you nothing. A rising scale does the job
// AND measures it: the operator hears which tones are missing, and each one is
// 500ms, so "I heard from the third tone" IS the open duration to within half a
// second. Every call becomes a measurement of the thing that has now produced
// three different symptoms from one cause.
//
// This belongs here and not in a consuming project's audio asset. The device
// open is this fake client's problem -- a real carrier has no ffplay and no
// cold open -- and padding the consumer's own prologue would cost every REAL
// call that dead air to fix a defect that exists only here.
const (
	// warmToneStep is how long each tone sounds. It is the resolution of the
	// measurement: shorter reads the open more precisely, longer is easier to
	// count by ear. 500ms is the compromise, and it divides 20ms exactly so
	// every tone is a whole number of frames and the scale stays on the frame
	// grid.
	warmToneStep = 500 * telephony.SampleRateHz / 1000 // samples per tone

	// warmToneAmplitude is well below full scale: this is a diagnostic played
	// into the operator's speakers at the start of every call, not a signal
	// anybody needs at volume.
	warmToneAmplitude = 0.25
)

// warmToneScale is six semitones from middle C -- C4, C#4, D4, D#4, E4, F4 --
// at equal temperament, A4 = 440Hz.
//
// Rising, so a missing prefix is audible as "it started partway up" rather than
// as a chord nobody can order by ear. Six at 500ms is 3.0s, which covers the
// ~2.5s open measured on 2026-09-11 with headroom; if an open ever eats the
// whole scale, that is itself the finding and the scale should grow.
//
// Every frequency is far below the 4kHz Nyquist limit of 8kHz mu-law, so none
// of them aliases.
var warmToneScale = []float64{
	261.63, // C4  (middle C)
	277.18, // C#4
	293.66, // D4
	311.13, // D#4
	329.63, // E4
	349.23, // F4
}

// warmToneFrames renders the scale as 8kHz mu-law.
//
// Each tone starts and ends at a zero crossing of its own envelope rather than
// being cut mid-cycle: a hard cut is a step discontinuity, which is a click,
// and a scale of six clicks is a worse diagnostic than no scale. The envelope
// is a 10ms raised-cosine fade at each end -- short enough not to blur the
// 500ms boundary the measurement depends on.
func warmToneFrames() []byte {
	const fadeSamples = telephony.SampleRateHz * 10 / 1000 // 10ms

	out := make([]byte, 0, len(warmToneScale)*warmToneStep)
	for _, hz := range warmToneScale {
		for i := 0; i < warmToneStep; i++ {
			env := 1.0
			switch {
			case i < fadeSamples:
				env = 0.5 * (1 - math.Cos(math.Pi*float64(i)/fadeSamples))
			case i >= warmToneStep-fadeSamples:
				j := warmToneStep - 1 - i
				env = 0.5 * (1 - math.Cos(math.Pi*float64(j)/fadeSamples))
			}
			v := warmToneAmplitude * env * math.Sin(2*math.Pi*hz*float64(i)/telephony.SampleRateHz)
			out = append(out, telephony.LinearToMuLaw(int16(v*math.MaxInt16)))
		}
	}
	return out
}

// playWarmTones hands the scale to the player and tells the filler about it.
//
// ONE function because they are one act. Played without the fed(), the filler
// sees a player it believes has been fed nothing and writes silence frames on
// top of the scale for as long as the scale lasts, interleaving the two and
// destroying the measurement. Those were two adjacent lines in dial until a
// mutation showed that deleting the second broke nothing any test could see --
// which is precisely the shape of a thing that gets separated later.
//
// Frame by frame, not as one blob: a single 3s write would enter the pipe in
// one call and bypass the pacing entirely, which is the burst shape this client
// exists to have stopped producing.
func playWarmTones(play func([]byte), f *playoutFiller, now time.Time) {
	warm := warmToneFrames()
	for off := 0; off < len(warm); off += muLawFrame20ms {
		end := off + muLawFrame20ms
		if end > len(warm) {
			end = len(warm)
		}
		play(warm[off:end])
	}
	f.fed(warm, now)
}
