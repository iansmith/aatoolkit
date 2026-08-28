package main

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// The inbound recorder answers one question the local speaker cannot: did the
// server's audio actually arrive, and when?
//
// It exists because of a real dead end. An operator on a demo call heard the
// server's recorded introduction and then nothing -- not the server's cue, not
// its spoken reply -- while the server's own logs showed both being generated
// and sent. Every layer in between could be blamed and none could be checked:
// twilio-cli logs control frames but never media, and the only other observer
// was a person listening to a speaker. Recording what the socket delivered
// separates "the bytes never came" from "the bytes came and nobody heard
// them", which are different faults with no overlap in their fixes.
//
// This is a diagnostic, off unless -record names a file. It writes what
// arrived, never what was played -- ffplay's own timing stays unobservable
// from here, and the sidecar's arrival timestamps are what stand in for it.
type inboundRecorder struct {
	mu      sync.Mutex
	audio   *os.File
	events  *os.File
	started time.Time
	total   int
	last    time.Time
}

// newInboundRecorder opens path for the raw μ-law stream and path+".jsonl" for
// per-arrival timing. A nil recorder is the off state and every method tolerates
// it, so the call path carries no conditional.
func newInboundRecorder(path string, now time.Time) (*inboundRecorder, error) {
	if path == "" {
		return nil, nil
	}
	audio, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("record: create %s: %w", path, err)
	}
	events, err := os.Create(path + ".jsonl")
	if err != nil {
		audio.Close()
		return nil, fmt.Errorf("record: create %s.jsonl: %w", path, err)
	}
	log.Printf("twilio-cli: recording inbound audio to %s (timing in %s.jsonl)", path, path)
	return &inboundRecorder{audio: audio, events: events, started: now, last: now}, nil
}

// writeIn records one inbound media payload.
//
// The sidecar carries a line per arrival with the offset into the call, the
// payload size, and the gap since the previous payload. The gap is the field
// worth having: a stream that stops for thirty seconds and resumes is
// indistinguishable from a continuous one in the audio file alone, and the
// gaps are exactly where a caller reports hearing nothing.
func (r *inboundRecorder) writeIn(payload []byte, now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	gap := now.Sub(r.last)
	r.last = now
	r.total += len(payload)

	if _, err := r.audio.Write(payload); err != nil {
		log.Printf("twilio-cli: record: write audio: %v", err)
	}
	fmt.Fprintf(r.events, `{"at_ms":%d,"bytes":%d,"gap_ms":%d,"total_bytes":%d}`+"\n",
		now.Sub(r.started).Milliseconds(), len(payload), gap.Milliseconds(), r.total)
}

// close finishes the recording and logs the one summary that makes the file
// interpretable: how much audio arrived against how long the call ran. A ratio
// far below 1 means the server sent less audio than the call had room for --
// the silences are real. A ratio near 1 means the audio was there and the
// fault is downstream of this process.
func (r *inboundRecorder) close(now time.Time) {
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
	log.Printf("twilio-cli: recorded %d bytes of inbound audio (%s of sound over a %s call, %.2f×)",
		r.total, audio.Round(time.Millisecond), wall.Round(time.Millisecond), ratio)

	if err := r.audio.Close(); err != nil {
		log.Printf("twilio-cli: record: close audio: %v", err)
	}
	if err := r.events.Close(); err != nil {
		log.Printf("twilio-cli: record: close timing: %v", err)
	}
}
