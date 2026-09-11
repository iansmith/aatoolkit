package main

import (
	"bytes"
	"sync/atomic"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// A laptop is not a telephone handset.
//
// twilio-cli plays inbound audio through the default output device and
// captures the default input device at the same time. With no headphones the
// microphone hears the speaker, and every sentence the server says goes back
// up the stream as caller speech: the server transcribes its own voice, barges
// in on itself, and answers what it just said. Measured on one demo call --
// every inbound turn was the preceding outbound one, with the operator's real
// words appended to the tail of the echo. A real call does not have this
// because the carrier cancels it, so the failure is specific to the fake-call
// harness, which is exactly where turn-taking regressions are meant to show.
//
// Real acoustic echo cancellation needs the playback signal as a reference,
// adaptive filtering and double-talk detection; ffmpeg ships nothing usable
// for it and the alternatives are a cgo dependency on a test client. Not worth
// it. Half-duplex gating is the right shape here, and the state it needs
// already exists: playoutFiller.outstanding answers precisely "how much audio
// has been handed to the player and not yet heard".
//
// So the gate is a deadline, not a switch. Whoever hands the player audio a
// microphone could hear publishes the wall-clock instant the player runs dry;
// shutUntil adds the allowance for the room still ringing, and the outbound
// frame path compares the result against now. The two live on different
// goroutines -- the filler is owned by
// dialReadLoop, the frame source runs off dial -- so the crossing is one
// atomic, which leaves playoutFiller itself single-owner and unsynchronised.
//
// "Audio a microphone could hear" excludes exactly one of the three feeders:
// playoutFiller.fill writes silence into the gap when the server has gone
// quiet, and silence is nothing to echo. The other two -- the server's media
// and the earcon tone -- both publish, through callAudio.fed.
//
// What it does NOT do is stop sending. The server's prologue paces its writes
// against inbound frames, so a client that goes quiet stalls the introduction
// and then trips the server's read timeout. The cadence is the contract; only
// the content of a frame changes.
type micGate struct {
	// shutUntilNanos is the UnixNano instant the mic reopens at, or 0 when the
	// gate is open. Written only by the read-loop goroutine (from wherever the
	// player is fed), read only by the frame source's.
	shutUntilNanos atomic.Int64
}

// micGateHangover is how long past the end of the player's queued audio the
// mic stays shut.
//
// It covers two things, which is why it is larger than a room-decay figure
// alone. The room is one: a microphone hears the tail of a word after the
// speaker has stopped moving. The other is that fedThrough counts bytes handed
// to ffplay's stdin, not bytes rendered -- twilio-cli cannot observe the
// player's own buffering and cannot flush that pipe (see playoutFiller.flush),
// so the horizon it derives is an estimate that runs slightly early.
//
// Too short is the failure that matters: the tail of the last word gets
// through, which is enough for the server's STT to produce a turn -- and one
// spurious turn is the whole defect, at a quieter volume.
const micGateHangover = 250 * time.Millisecond

func newMicGate() *micGate { return &micGate{} }

// shutUntil holds the mic shut through horizon -- the instant the player runs
// out of audio -- plus the hangover.
//
// The hangover is added here rather than by the caller because how long the
// room rings, and how far ahead of the speaker the player's pipe runs, are the
// gate's business and nobody else's. A publisher that had to remember to add
// it is a publisher that can forget.
//
// Nil-tolerant, like every method here: --full-duplex is a nil gate, and the
// feed path publishes on every frame the player is handed. A caller that had
// to check first would be one `if` away from the panic on whichever branch it
// forgot -- and the branch it would forget is the earcon's.
func (g *micGate) shutUntil(horizon time.Time) {
	if g == nil {
		return
	}
	g.shutUntilNanos.Store(horizon.Add(micGateHangover).UnixNano())
}

// open reopens the mic now, whatever deadline was standing.
//
// This is what a Twilio clear means here: the server abandoned the rest of the
// reply, so the audio the deadline was derived from will never be heard, and
// waiting it out would silence the caller through the one moment -- their
// barge-in -- the harness most needs to record.
//
// It reopens over a residual echo window, deliberately. A clear drops the
// player's own queue too (audioPlayer.flush), but nothing can reach the bytes
// already written into ffplay's stdin -- so for as long as that pipe takes to
// drain, the speaker is still playing the abandoned reply with the mic live.
// Recording the barge-in is worth that; recording nothing is not.
func (g *micGate) open() {
	if g == nil {
		return
	}
	g.shutUntilNanos.Store(0)
}

// shut reports whether the mic is gated right now. A nil gate is never shut.
func (g *micGate) shut() bool {
	return g != nil && time.Now().UnixNano() < g.shutUntilNanos.Load()
}

// gated returns the frame that should go out in place of payload: mu-law
// silence of the same size while the gate is shut, and payload itself
// otherwise.
//
// Same size, and always a frame: a server that paces its writes against
// inbound frames stalls if the client stops sending, so the cadence is the
// contract and only the content may change.
//
// A nil gate is full duplex: --full-duplex is the absence of a gate rather
// than a flag the gate consults, so there is nothing to keep in step.
func (g *micGate) gated(payload []byte) []byte {
	if !g.shut() {
		return payload
	}
	return bytes.Repeat([]byte{telephony.MuLawSilence}, len(payload))
}
