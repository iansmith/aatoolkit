package driver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// captureUtterance records one spoken turn from the microphone and returns it as
// a 16 kHz mono WAV, ending the turn on the silence-gated "Done" gesture
// (design §2-§4): speak, pause ~armSilence, say a stopword. It shells out to
// ffmpeg (no cgo) and drives the pure `endpointer` state machine from ffmpeg's
// silencedetect events.
//
//   - ffmpeg's silence_start/silence_end log lines are turned into alternating
//     voiced/unvoiced VAD spans (no separate VAD dependency).
//   - On a "Done" candidate the buffer-so-far is transcribed and its trailing
//     word checked against the stopwords (the design §4 double-duty pass).
//   - On end, the buffer is cut at the arming-pause start (wavHead), dropping the
//     "Done" clip and anything captured after it (the in-car passenger case).
//
// Robustness carried over from SOP-2: a wall-clock context deadline + Cancel=
// SIGINT + WaitDelay=SIGKILL guarantee ffmpeg always exits (BT stalls/teardown),
// and a post-capture level check rejects silence.
func captureUtterance() ([]byte, error) {
	mic := EnvOr("AATOOLKIT_STT_MIC", ":default") // avfoundation; ":default" = the system input device
	noise := EnvOr("AATOOLKIT_STT_NOISE", "-40dB")
	arm := msDur(EnvFloatOr("AATOOLKIT_ARM_SILENCE_MS", 2000))
	abs := msDur(EnvFloatOr("AATOOLKIT_ABS_SILENCE_MS", 12000))
	maxDone := msDur(EnvFloatOr("AATOOLKIT_STT_MAXDONE_MS", 1000))
	maxSec := EnvFloatOr("AATOOLKIT_STT_MAX_SEC", 180.0)
	quietDBFS := EnvFloatOr("AATOOLKIT_STT_MIN_DBFS", -55.0)
	stopwords := splitStopwords(EnvOr("AATOOLKIT_STT_STOPWORDS", "done,stop"))
	sttURL := EnvOr("AATOOLKIT_STT_URL", "http://127.0.0.1:7789/v1/audio/transcriptions")
	const detectSilenceSec = 0.3 // silencedetect granularity: fine enough to see the pauses

	// User-facing turn cue on stdout (the previous stderr line was too easy to
	// miss). It's the spoken mirror of a text prompt: it tells the user it's their
	// turn to talk. Keeps the mic name — the wrong-device case is the main failure
	// mode, and seeing which device is live makes it obvious.
	fmt.Printf("🎙  Listening on %s — speak now, then pause ~%.0fs and say \"done\" to finish.\n", mic, arm.Seconds())

	f, err := os.CreateTemp("", "stt-*.wav")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(maxSec+3)*time.Second)
	defer cancel()

	events := make(chan vadEvent, 64)
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "info",
		"-f", "avfoundation", "-i", mic,
		"-ar", "16000", "-ac", "1",
		"-af", fmt.Sprintf("silencedetect=noise=%s:d=%.2f", noise, detectSilenceSec),
		"-t", strconv.FormatFloat(maxSec, 'f', -1, 64),
		"-y", tmp,
	)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	cmd.Stderr = &vadParser{out: events}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg (installed? `brew install ffmpeg`): %w", err)
	}

	// The endpointer runs in one goroutine (its state needs no lock); the ffmpeg
	// stderr goroutine only feeds VAD events. On end it reports the media-time cut
	// point and stops ffmpeg.
	ep := newEndpointer(arm, abs, maxDone)
	ended := make(chan float64, 1)
	go runEndpointer(ctx, cancel, ep, events, ended, &http.Client{}, sttURL, tmp, stopwords)

	_ = cmd.Wait() // returns on the goroutine's cancel (turn ended), -t, or the deadline
	cancel()       // if ffmpeg stopped on its own, unblock the goroutine
	cutSec := <-ended

	full, err := os.ReadFile(tmp)
	if err != nil {
		return nil, err
	}
	out := full
	if cutSec > 0 {
		if h := wavHead(full, secDur(cutSec)); h != nil {
			out = h
		}
	}
	if !loudEnough(out, quietDBFS) {
		return nil, errNoSpeech
	}
	return out, nil
}

// runEndpointer feeds VAD spans (reconstructed from silencedetect events, plus a
// 100 ms tick for ongoing trailing silence) into the endpointer and acts on its
// decisions. On the "Done" candidate it transcribes the buffer-so-far and checks
// the trailing word; on end it reports the cut point on `ended` and stops ffmpeg.
func runEndpointer(ctx context.Context, cancel context.CancelFunc, ep *endpointer,
	events <-chan vadEvent, ended chan<- float64, client *http.Client, sttURL, tmp string, stopwords []string) {

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var prevStart, curStart, lastEnd float64 // silencedetect media times (seconds)
	inSilence := false
	cut := 0.0

	for {
		var dec endDecision
		select {
		case <-ctx.Done():
			ended <- cut
			return
		case ev := <-events:
			if ev.voiced { // silence_end: speech resumed; the trailing silence was fed by ticks
				lastEnd = ev.mediaT
				inSilence = false
				continue
			}
			// silence_start: the voiced span before it just ended
			prevStart, curStart = curStart, ev.mediaT
			inSilence = true
			dec = ep.onSpan(true, secDur(ev.mediaT-lastEnd))
		case <-ticker.C:
			if !inSilence {
				continue
			}
			dec = ep.onSpan(false, 100*time.Millisecond)
		}

		switch dec {
		case confirmCandidate:
			cut = prevStart // turn content ends at the arming-pause start (before "Done")
			isStop := trailingStopwordFromFile(client, sttURL, tmp, stopwords)
			if isStop {
				// Confirm the gesture landed, so the user knows the turn is ending
				// and doesn't keep waiting or re-say "done".
				fmt.Println("✋  Stop word detected — got it.")
			}
			dec = ep.confirmed(isStop)
		case endTurn:
			cut = curStart // absolute-silence fallback: cut at the end of speech
		}
		if dec == endTurn {
			ended <- cut
			cancel() // stop ffmpeg so the main Wait returns promptly
			return
		}
	}
}

// parseSilenceStart pulls the timestamp out of an ffmpeg silencedetect
// "silence_start: <seconds>" log line. Returns (0,false) for any other line.
func parseSilenceStart(line string) (float64, bool) {
	return parseSilenceField(line, "silence_start:")
}

// parseSilenceField pulls the first numeric field after marker out of an ffmpeg
// silencedetect log line. Returns (0,false) for any other line.
func parseSilenceField(line, marker string) (float64, bool) {
	_, after, found := strings.Cut(line, marker)
	if !found {
		return 0, false
	}
	fields := strings.Fields(after)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// EnvFloatOr reads a float env var, falling back to def when unset or unparseable.
func EnvFloatOr(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// --- SOP-2 item 5: mic robustness ---

// errNoSpeech signals that a capture contained no usable speech (silence, a
// wrong/dead device, or a stalled input). The caller treats it as an empty turn,
// not a failure — so we never spend a transcription on silence.
var errNoSpeech = errors.New("no speech detected")

// loudEnough reports whether a 16-bit PCM WAV has a peak louder than
// thresholdDBFS (0 = full scale, negative = quieter). Used to reject
// silent/stalled captures before transcribing them.
func loudEnough(wav []byte, thresholdDBFS float64) bool {
	data, ok := wavData(wav)
	if !ok || len(data) < 2 {
		return false
	}
	var peak int32
	for i := 0; i+1 < len(data); i += 2 {
		s := int32(int16(binary.LittleEndian.Uint16(data[i:])))
		if s < 0 {
			s = -s // widened to int32 first, so -(-32768) = 32768 (no int16 overflow)
		}
		if s > peak {
			peak = s
		}
	}
	if peak == 0 {
		return false
	}
	return 20*math.Log10(float64(peak)/32768.0) >= thresholdDBFS
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

// --- SOP-3: silence-gated "Done" endpointing ---

// vadEvent is one ffmpeg silencedetect boundary. voiced=true is a silence_end
// (speech resumed); voiced=false is a silence_start (silence began). mediaT is
// ffmpeg's media timestamp in seconds.
type vadEvent struct {
	voiced bool
	mediaT float64
}

// vadParser is an io.Writer for ffmpeg's stderr that emits a vadEvent for each
// silencedetect silence_start / silence_end line (handling split lines).
type vadParser struct {
	out chan<- vadEvent
	buf []byte
}

func (p *vadParser) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := string(p.buf[:i])
		p.buf = p.buf[i+1:]
		if t, ok := parseSilenceStart(line); ok {
			p.out <- vadEvent{voiced: false, mediaT: t}
		} else if t, ok := parseSilenceEnd(line); ok {
			p.out <- vadEvent{voiced: true, mediaT: t}
		}
	}
	return len(b), nil
}

// parseSilenceEnd pulls the end timestamp out of an ffmpeg silencedetect
// "silence_end: <seconds> | silence_duration: ..." line.
func parseSilenceEnd(line string) (float64, bool) {
	return parseSilenceField(line, "silence_end:")
}

// trailingStopwordFromFile transcribes the recording-so-far and reports whether
// its trailing word is a stopword — the design §4 "double-duty" confirm pass.
func trailingStopwordFromFile(client *http.Client, sttURL, tmp string, stopwords []string) bool {
	raw, err := os.ReadFile(tmp)
	if err != nil {
		return false
	}
	pcm := wavStreamPCM(raw)
	if pcm == nil {
		return false
	}
	text, err := transcribe(client, sttURL, buildWAV16kMono(pcm))
	if err != nil {
		return false
	}
	return trailingStopword(text, stopwords)
}

// trailingStopword reports whether the last word of text is a stopword.
func trailingStopword(text string, stopwords []string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	return isStopword(fields[len(fields)-1], stopwords)
}

// wavStreamPCM returns the PCM bytes of a WAV's "data" chunk to EOF, ignoring the
// chunk's size field (which is still a placeholder in an in-progress recording).
func wavStreamPCM(wav []byte) []byte {
	if len(wav) < 12 || string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil
	}
	for i := 12; i+8 <= len(wav); {
		id := string(wav[i : i+4])
		sz := int(binary.LittleEndian.Uint32(wav[i+4 : i+8]))
		body := i + 8
		if id == "data" {
			return wav[body:]
		}
		if sz <= 0 || body+sz > len(wav) {
			break
		}
		i = body + sz + (sz & 1)
	}
	return nil
}

func splitStopwords(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func msDur(ms float64) time.Duration   { return time.Duration(ms) * time.Millisecond }
func secDur(sec float64) time.Duration { return time.Duration(sec * float64(time.Second)) }
