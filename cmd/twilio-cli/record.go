package main

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// A stream recorder answers, for one direction of a call, a question nothing
// else in this process can: what audio actually crossed the socket, and when?
//
// The inbound direction exists because of a real dead end. An operator on a
// demo call heard the server's recorded introduction and then nothing -- not
// the server's cue, not its spoken reply -- while the server's own logs showed
// both being generated and sent. Every layer in between could be blamed and
// none could be checked: twilio-cli logs control frames but never media, and
// the only other observer was a person listening to a speaker. Recording what
// the socket delivered separates "the bytes never came" from "the bytes came
// and nobody heard them", which are different faults with no overlap in their
// fixes.
//
// The outbound direction exists for a different reason: the caller's audio only
// ever existed in flight, so a live conversation with one model could never be
// put to a second one. Recording the bytes sent makes the caller's half of a
// call a file -- and the file it writes is exactly what -audio replays, so the
// same words reach the second model rather than a fresh performance of them.
//
// Both are diagnostics, off unless their flag names a file. Each writes what
// crossed the socket, never what was heard or spoken: ffplay's timing and the
// microphone's are unobservable from here, and the sidecar's timestamps are
// what stand in for them.
type streamRecorder struct {
	kind    recorderKind
	mu      sync.Mutex
	audio   *os.File
	events  *os.File
	started time.Time
	total   int
	last    time.Time
}

// recorderKind is the one direction-dependent thing about a recorder: the words
// it uses when it talks to the operator. The flag name rides along with the
// direction because a failure to open the file must name the flag that asked
// for it -- with two recorders live, "record: create ..." names neither.
type recorderKind struct {
	dir  string // "inbound" / "outbound", for log prose
	flag string // the flag that turns this direction on
}

var (
	recordInbound  = recorderKind{dir: "inbound", flag: "-record"}
	recordOutbound = recorderKind{dir: "outbound", flag: "-record-sent"}
)

// newStreamRecorder opens path for the raw μ-law stream and path+".jsonl" for
// per-payload timing. A nil recorder is the off state and every method tolerates
// it, so the call path carries no conditional.
func newStreamRecorder(kind recorderKind, path string, now time.Time) (*streamRecorder, error) {
	if path == "" {
		return nil, nil
	}
	audio, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("%s: create %s: %w", kind.flag, path, err)
	}
	events, err := os.Create(path + ".jsonl")
	if err != nil {
		audio.Close()
		return nil, fmt.Errorf("%s: create %s.jsonl: %w", kind.flag, path, err)
	}
	log.Printf("twilio-cli: recording %s audio to %s (timing in %s.jsonl)", kind.dir, path, path)
	return &streamRecorder{kind: kind, audio: audio, events: events, started: now, last: now}, nil
}

// write records one media payload.
//
// The sidecar carries a line per payload with the offset into the call, the
// size, and the gap since the previous one. The gap is the field worth having:
// a stream that stops for thirty seconds and resumes is indistinguishable from
// a continuous one in the audio file alone, and the gaps are exactly where a
// caller reports hearing nothing.
func (r *streamRecorder) write(payload []byte, now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	gap := now.Sub(r.last)
	r.last = now
	r.total += len(payload)

	if _, err := r.audio.Write(payload); err != nil {
		log.Printf("twilio-cli: %s: write audio: %v", r.kind.flag, err)
	}
	// Checked, like the audio write above. The sidecar is half the reason
	// recording exists -- the audio alone cannot show a thirty-second gap -- so a
	// full disk losing it silently would leave an operator reading a truncated
	// file as evidence about the call.
	if _, err := fmt.Fprintf(r.events, `{"at_ms":%d,"bytes":%d,"gap_ms":%d,"total_bytes":%d}`+"\n",
		now.Sub(r.started).Milliseconds(), len(payload), gap.Milliseconds(), r.total); err != nil {
		log.Printf("twilio-cli: %s: write timing: %v", r.kind.flag, err)
	}
}

// close finishes the recording and logs the one summary that makes the file
// interpretable: how much audio crossed the socket against how long the call
// ran. A ratio far below 1 means less audio moved than the call had room for --
// the silences are real. A ratio near 1 means the audio was there and the fault
// is downstream of this process.
func (r *streamRecorder) close(now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	wall := now.Sub(r.started)
	audio := mulawPlayoutDuration(r.total)
	ratio := 0.0
	if wall > 0 {
		ratio = audio.Seconds() / wall.Seconds()
	}
	log.Printf("twilio-cli: recorded %d bytes of %s audio (%s of sound over a %s call, %.2f×)",
		r.total, r.kind.dir, audio.Round(time.Millisecond), wall.Round(time.Millisecond), ratio)

	if err := r.audio.Close(); err != nil {
		log.Printf("twilio-cli: %s: close audio: %v", r.kind.flag, err)
	}
	if err := r.events.Close(); err != nil {
		log.Printf("twilio-cli: %s: close timing: %v", r.kind.flag, err)
	}
}
