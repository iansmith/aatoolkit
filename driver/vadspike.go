package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// RunVADSpike is a throwaway harness (untracked spike — see
// design/voice-input-stt.md §5) that proves out dual-threshold VAD
// segmentation: a short "phrase break" pause fires separately from, and
// earlier than, the longer "end of utterance" pause, so both can be
// validated against spoken test phrases by watching the printed log in real
// time. It does not transcribe anything or wire into the driver loop — it
// only detects and reports the two boundaries.
//
// The phrase-break threshold is adaptive, not fixed: a bare wake phrase
// ("Hey there") followed by a pause should NOT count as a phrase break — at
// that point we likely have no real content yet, just the attention word.
// So the threshold starts high (little content captured yet → require a
// longer pause before treating it as a boundary) and ramps down as more
// speech accumulates (once there's real content, a shorter pause is a
// trustworthy clause boundary worth an early Whisper pass on). The ramp's
// x-axis is accumulated VOICED duration within the utterance, not wall-clock
// elapsed time — voiced duration is a better proxy for "how much content have
// we captured" since it isn't inflated by the pauses themselves.
func RunVADSpike() error {
	mic := EnvOr("AATOOLKIT_STT_MIC", ":default")
	noise := EnvOr("AATOOLKIT_STT_NOISE", "-40dB")
	turnEnd := msDur(EnvFloatOr("AATOOLKIT_VAD_END_MS", 1500))
	ramp := phraseRamp{
		startMS:           EnvFloatOr("AATOOLKIT_VAD_PHRASE_START_MS", 650),
		endMS:             EnvFloatOr("AATOOLKIT_VAD_PHRASE_END_MS", 200),
		voicedRampStartMS: EnvFloatOr("AATOOLKIT_VAD_RAMP_VOICED_START_MS", 800),
		voicedRampEndMS:   EnvFloatOr("AATOOLKIT_VAD_RAMP_VOICED_END_MS", 1500),
	}
	const detectSilenceSec = 0.05 // fine granularity so both thresholds see every pause

	fmt.Printf("🎙  VAD dual-threshold spike — mic=%s noise=%s turnEnd=%s\n", mic, noise, turnEnd)
	fmt.Printf("    phrase-break threshold ramps %.0fms → %.0fms as voiced speech goes %.0fms → %.0fms\n",
		ramp.startMS, ramp.endMS, ramp.voicedRampStartMS, ramp.voicedRampEndMS)
	fmt.Println("    Speak a test phrase with a natural pause in it, e.g. \"Hey there <pause> we've got a problem with soccer practice today\". Ctrl+C to quit.")

	for {
		if err := vadSpikeOneUtterance(mic, noise, ramp, turnEnd, detectSilenceSec); err != nil {
			return err
		}
	}
}

// vadTape accumulates the whole mic stream (16 kHz mono s16le) that ffmpeg
// writes to stdout, so a slice of it can be dumped as a WAV once a boundary
// fires. Deliberately unbounded — the spike's utterances are capped at the
// 60s ffmpeg context timeout, and holding ~2 MB of PCM is not worth the
// complexity of a ring buffer here.
type vadTape struct {
	mu  sync.Mutex
	pcm []byte
}

func (t *vadTape) Write(b []byte) (int, error) {
	t.mu.Lock()
	t.pcm = append(t.pcm, b...)
	t.mu.Unlock()
	return len(b), nil
}

// span returns the PCM between two media timestamps, clamped to what has
// actually arrived from ffmpeg so far.
func (t *vadTape) span(fromSec, toSec float64) []byte {
	const bytesPerSec = 16000 * 2

	t.mu.Lock()
	defer t.mu.Unlock()

	lo := int(fromSec * bytesPerSec)
	hi := int(toSec * bytesPerSec)
	lo -= lo % 2 // whole samples
	hi -= hi % 2
	if lo < 0 {
		lo = 0
	}
	if hi > len(t.pcm) {
		hi = len(t.pcm)
	}
	if lo >= hi {
		return nil
	}
	return append([]byte(nil), t.pcm[lo:hi]...)
}

// dumpFirstPortion writes the audio between utterance start and the first
// phrase break to a WAV so the segmentation can be checked by ear. It runs on
// its own goroutine (the caller's ticker loop must keep counting trailing
// silence) and pauses briefly first: the phrase break fires off the ticker,
// which can run ahead of the PCM ffmpeg has actually flushed to us.
func dumpFirstPortion(tape *vadTape, fromSec, toSec float64) {
	path := fmt.Sprintf("/tmp/vadspike-first-portion-%d.wav", time.Now().UnixNano()/1e6)
	fmt.Printf("           💾 outputting first utterance portion (t=%.2fs → %.2fs) to %s ...\n", fromSec, toSec, path)

	time.Sleep(300 * time.Millisecond) // let ffmpeg's stdout catch up to toSec

	pcm := tape.span(fromSec, toSec)
	if len(pcm) == 0 {
		fmt.Printf("           ⚠️  nothing to write — no PCM captured for t=%.2fs → %.2fs\n", fromSec, toSec)
		return
	}
	if err := os.WriteFile(path, buildWAV16kMono(pcm), 0o644); err != nil {
		fmt.Printf("           ⚠️  writing %s: %v\n", path, err)
		return
	}
	fmt.Printf("           ✅ done writing first utterance portion (%.2fs of audio, %d bytes) — %s\n",
		float64(len(pcm))/(16000*2), len(pcm), path)
}

// phraseRamp computes the current phrase-break silence threshold as a
// function of voiced duration accumulated so far in the utterance: flat at
// startMS up to voicedRampStartMS of voiced speech, linearly interpolating
// down to endMS by voicedRampEndMS, flat at endMS thereafter.
type phraseRamp struct {
	startMS, endMS                     float64
	voicedRampStartMS, voicedRampEndMS float64
}

func (r phraseRamp) threshold(totalVoiced time.Duration) time.Duration {
	v := float64(totalVoiced.Milliseconds())
	switch {
	case v <= r.voicedRampStartMS:
		return msDur(r.startMS)
	case v >= r.voicedRampEndMS:
		return msDur(r.endMS)
	default:
		frac := (v - r.voicedRampStartMS) / (r.voicedRampEndMS - r.voicedRampStartMS)
		return msDur(r.startMS + frac*(r.endMS-r.startMS))
	}
}

// vadSpikeOneUtterance runs ffmpeg's silencedetect against the mic (audio
// itself is discarded — this spike only cares about the silence boundaries)
// and drives trailing-silence accumulation off a fast ticker, exactly like
// runEndpointer's tick-based trailing-silence tracking. Voiced-span duration
// is measured the same way endpoint.go's production onSpan(true, d) call
// does: at each silence_start, d = mediaT_of_silence_start - lastEnd, which
// correctly covers the FIRST voiced span too (lastEnd starts at 0). An
// earlier version of this spike gated on a "spoken" flag set only on speech
// *resume* events, which meant the very first pause in a recording never
// accumulated trailing silence — fixed here.
func vadSpikeOneUtterance(mic, noise string, ramp phraseRamp, turnEnd time.Duration, detectSilenceSec float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	events := make(chan vadEvent, 64)
	tape := &vadTape{}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "info",
		"-f", "avfoundation", "-i", mic,
		"-ar", "16000", "-ac", "1",
		"-af", fmt.Sprintf("silencedetect=noise=%s:d=%.2f", noise, detectSilenceSec),
		"-f", "s16le", "-", // silencedetect passes audio through; keep it so we can dump a clip
	)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	cmd.Stderr = &vadParser{out: events}
	cmd.Stdout = tape

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg (installed? `brew install ffmpeg`): %w", err)
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var trailing, totalVoiced time.Duration
	var pauseStart, lastEnd float64
	inSilence := false
	phraseFired := false
	dumped := false    // only the FIRST phrase break of an utterance gets written out
	utterStart := -1.0 // media time of the first voiced sample; -1 until speech starts

	fmt.Println("—— listening for a new utterance ——")

	for {
		select {
		case <-ctx.Done():
			_ = cmd.Wait()
			return nil
		case ev := <-events:
			if ev.voiced { // silence_end: speech resumed
				if inSilence {
					fmt.Printf("[t=%6.2fs] 🎤 speech resumed (pause lasted %s, %s voiced so far)\n",
						ev.mediaT, trailing.Round(10*time.Millisecond), totalVoiced.Round(10*time.Millisecond))
				}
				if totalVoiced == 0 {
					utterStart = ev.mediaT // first speech of this utterance
				}
				lastEnd = ev.mediaT
				inSilence = false
				trailing = 0
				phraseFired = false
				continue
			}
			// silence_start: the voiced span before it (from lastEnd to now) just ended
			totalVoiced += secDur(ev.mediaT - lastEnd)
			pauseStart = ev.mediaT
			inSilence = true
		case <-ticker.C:
			if !inSilence || totalVoiced == 0 {
				continue
			}
			trailing += 50 * time.Millisecond
			now := pauseStart + trailing.Seconds()
			phraseBreak := ramp.threshold(totalVoiced)
			if !phraseFired && trailing >= phraseBreak {
				phraseFired = true
				fmt.Printf("[t=%6.2fs] 🔹 PHRASE BREAK — silence reached %s (threshold at %s voiced; pause started t=%.2fs)\n",
					now, trailing.Round(10*time.Millisecond), totalVoiced.Round(10*time.Millisecond), pauseStart)
				if !dumped {
					dumped = true
					from := utterStart
					if from < 0 {
						from = 0 // speech started at t=0, before any silence_end
					}
					go dumpFirstPortion(tape, from, pauseStart)
				}
			}
			if trailing >= turnEnd {
				fmt.Printf("[t=%6.2fs] ⏹  END OF UTTERANCE — silence reached %s\n", now, turnEnd)
				cancel()
			}
		}
	}
}
