package main

import "time"

// A call is a real-time stream; a pipe into ffplay is not.
//
// twilio-cli hands inbound media straight to one long-lived ffplay reading raw
// μ-law from stdin. That stream carries no timing of its own -- position in it
// is byte offset, nothing more -- so the only thing keeping playback aligned
// with the call is that bytes happen to arrive at the rate they are consumed.
// A conversation breaks that assumption twice over: the server sends nothing
// at all while it is listening or thinking, and then sends a whole utterance
// faster than real time. Measured on one demo call: 100 s of introduction
// arrived paced, then NOTHING for 47 seconds, then 4.9 s of speech in 2.4 s.
//
// Feeding that to a player as an unbroken byte stream asks it to render a
// silence that is not in the bytes. What the caller heard was the
// introduction, and then nothing further for the rest of the call -- while the
// recorded socket capture showed the reply arriving intact and loud.
//
// The filler makes the stream say what the call means: during a gap it writes
// real silence at real time, so the player keeps running and stays level with
// the wall clock instead of stalling at whatever position the last packet
// left. aatoolkit's own audio Tap does the same thing for the same reason --
// see Tap.DrainOut, whose silence exists so a recording's timeline matches the
// call's.
//
// It is deliberately NOT a jitter buffer. It adds no latency and reorders
// nothing; it only refuses to let a silent stretch of call become a missing
// stretch of stream.
type playoutFiller struct {
	// fedThrough is the wall-clock instant up to which audio has been handed
	// to the player. Ahead of now when the server has sent faster than real
	// time, which is normal and needs no filling; behind now exactly when the
	// server has gone quiet.
	fedThrough time.Time

	// frame is one 20 ms μ-law silence frame, the unit both directions of a
	// call already use.
	frame []byte
}

func newPlayoutFiller(now time.Time, silence byte) *playoutFiller {
	frame := make([]byte, muLawFrame20ms)
	for i := range frame {
		frame[i] = silence
	}
	return &playoutFiller{fedThrough: now, frame: frame}
}

// fed records that payload has been handed to the player.
//
// It advances fedThrough from whichever is later, now or the previous
// fedThrough: from now when the stream had gone quiet (the gap is over, and
// the audio being written starts here), and from fedThrough when the server is
// running ahead of real time (the new audio queues behind what is already
// there). Taking only one of the two would either double-count a burst or
// silently swallow a gap.
func (f *playoutFiller) fed(payload []byte, now time.Time) {
	if f.fedThrough.Before(now) {
		f.fedThrough = now
	}
	f.fedThrough = f.fedThrough.Add(mulawPlayoutDuration(len(payload)))
}

// fill writes silence frames until the player has been fed up to now, and
// reports how many it wrote.
//
// Whole frames only: a partial frame would put the stream off the 20 ms grid
// every other call, and the rounding error is at most one frame, which is
// recovered on the next fill.
func (f *playoutFiller) fill(now time.Time, play func([]byte)) int {
	// Past this much deficit, resync instead of catching up frame by frame.
	//
	// A laptop that slept, or a loop stalled behind a slow write, can come back
	// minutes behind. Filling that honestly means pushing minutes of silence
	// into ffplay's 64KB stdin pipe in one loop; ffplay drains it at 8000 B/s,
	// so play() blocks and takes the only websocket reader with it -- turning a
	// stall into a hang, which is strictly worse than the gap it was covering.
	//
	// A skip loses the illusion of continuity for one moment. A hang loses the
	// call.
	if now.Sub(f.fedThrough) > maxFillCatchUp {
		f.fedThrough = now
		return 0
	}

	n := 0
	for f.fedThrough.Add(mulawPlayoutDuration(len(f.frame))).Compare(now) <= 0 {
		play(f.frame)
		f.fedThrough = f.fedThrough.Add(mulawPlayoutDuration(len(f.frame)))
		n++
	}
	return n
}

// maxFillCatchUp bounds how much silence one fill may write. Two seconds is far
// above any scheduling jitter the 20ms tick produces and far below the point
// where writing it would block on the player.
const maxFillCatchUp = 2 * time.Second

// outstanding is how much handed-over audio has not played yet.
//
// This is what a mark echo must wait for. A mark asks "tell me when everything
// before this has played", and fedThrough is by construction the wall-clock
// instant through which audio has been supplied -- so the answer is simply how
// far that is still ahead of now.
//
// It replaces an arithmetic that measured from the previous mark and so
// charged silence preceding the audio against it; see
// TestPlayoutFiller_OutstandingIsWhatHasNotPlayedYet for the shape that broke.
//
// Zero when the stream has caught up, and never negative: a player that is
// idle has nothing queued, however long it has been idle.
func (f *playoutFiller) outstanding(now time.Time) time.Duration {
	if remaining := f.fedThrough.Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}
