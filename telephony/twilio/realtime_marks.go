package twilio

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// MarkEcho is one mark the engine reports back to the consumer: the carrier
// has finished playing everything that was written ahead of it.
//
// It carries a bool beside the name for the same reason CarrierAudio does
// rather than being a bare string: there are two ways a consumer's wait ends,
// and conflating them would leave it unable to tell "the carrier said this
// played" from "the carrier said nothing and the engine stopped waiting".
//
// TimedOut == false means the carrier echoed the mark, which is the signal
// this exists to deliver. TimedOut == true means it did not, and the engine's
// own bound elapsed instead — see carrierMediaSink.markEchoBound. A consumer
// hanging up after a spoken line can treat both as "go ahead": the second is
// the classic path's behavior too (handleMarkEchoTimeout closes anyway, logging
// that the peer did not honor the mark protocol). A consumer that cares which
// happened has the bool.
type MarkEcho struct {
	Name     string
	TimedOut bool
}

// playoutClock models the carrier as a real-time player, so the engine can say
// how much of what it has written has not been heard yet.
//
// It is the same model cmd/twilio-cli's playoutFiller keeps for the same
// quantity, reduced to the part a mark bound needs: no silence filling, no
// player, just the instant at which everything written so far will have
// finished. Folding the two onto one type would mean moving playoutFiller out
// of package main, which is a structural change and out of scope here
// (CLAUDE.md #4).
//
// Not safe for concurrent use. Every method is called under
// carrierMediaSink.writeSem, which is what makes the figure it reports
// consistent with the order the writes actually went out in.
type playoutClock struct {
	// through is the wall-clock instant up to which audio has been handed to
	// the carrier. Ahead of now when the backend has sent faster than real
	// time, which is the normal case; equal to now when the carrier has caught
	// up and nothing is queued.
	through time.Time
}

// fed records that n bytes of μ-law audio have been written to the carrier.
//
// It advances from whichever is later, now or the previous through: from now
// when nothing was queued (the audio being written starts playing here), and
// from through when the backend is running ahead of real time (the new audio
// queues behind what is already there). Taking only one of the two would
// either double-count a burst or lose a gap — the reasoning playoutFiller.fed
// records in full.
func (p *playoutClock) fed(n int, now time.Time) {
	if p.through.Before(now) {
		p.through = now
	}
	p.through = p.through.Add(telephony.MuLawDuration(n))
}

// flush drops the queued playout: this is what a Twilio clear means. Audio
// already written but not yet heard is abandoned, so nothing derived from
// through goes on describing audio nobody will hear — a mark written after a
// clear is owed the grace and nothing more.
func (p *playoutClock) flush(now time.Time) {
	p.through = now
}

// outstanding reports how much written audio has not played yet, floored at
// zero: a carrier that has caught up is not owed negative time.
func (p *playoutClock) outstanding(now time.Time) time.Duration {
	if d := p.through.Sub(now); d > 0 {
		return d
	}
	return 0
}

// markTracker owns the marks the engine has written to the carrier and not yet
// seen echoed. It is what makes the echo NAMED rather than merely counted: a
// consumer may have more than one mark in flight, and an echo carrying a name
// nothing is waiting on must not be read as the one that is.
//
// Both goroutines that touch a mark reach it through here — the select loop
// arms one after writing it, pumpCarrierToBridge's goroutine resolves the echo
// — so the map is mutex-guarded rather than owned by one loop, which is the
// exception on this path and the reason it is a type of its own.
//
// A nil *markTracker is the "consumer asked for no marks" case and every
// method tolerates it: HandleStreamRealtime builds one only when a mark option
// was actually supplied, so a call with neither option keeps today's behavior
// exactly — no mark is written, and an inbound mark frame is ignored without
// even a log line.
type markTracker struct {
	// echoCh is the consumer's destination, resolved once per call, and nil
	// when the consumer asked for none (it may still have asked for the
	// request half). The engine NEVER closes it.
	echoCh chan<- MarkEcho

	mu sync.Mutex
	// outstanding maps each written-but-unechoed mark name to the timer
	// enforcing its bound.
	outstanding map[string]*time.Timer
	dropped     int
	// stopped is set when the call ends. It is what keeps a timer that fires,
	// or an echo that arrives, during teardown from delivering on a channel
	// whose consumer has already been told the call is over.
	stopped bool
}

func newMarkTracker(echoCh chan<- MarkEcho) *markTracker {
	return &markTracker{echoCh: echoCh, outstanding: make(map[string]*time.Timer)}
}

// arm records name as outstanding and starts its bound.
//
// Called with carrierMediaSink.writeSem held, immediately after the mark's own
// write: the bound is derived from the playout queued ahead of that write, so
// arming outside the slot would let a media frame written in between change the
// figure the bound was computed from.
//
// A name that is already outstanding is logged and re-armed rather than
// refused. Two marks with one name are indistinguishable in the echo — the
// wire carries only the name — so there is nothing to be gained by keeping
// both, and the later bound is the one that covers the later audio.
func (t *markTracker) arm(name string, bound time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	if old, ok := t.outstanding[name]; ok {
		log.Printf("twilio: realtime: mark %q was already outstanding; re-arming its bound to %s", name, bound)
		old.Stop()
	}
	t.outstanding[name] = time.AfterFunc(bound, func() { t.expire(name) })
}

// echo resolves an inbound mark echo from the carrier.
//
// An echo matching nothing outstanding is logged and NOT delivered — the
// distinction handleMarkEchoControlEvent already draws on the classic path,
// and what stops a consumer with two marks in flight from reading one as the
// other. Called from pumpCarrierToBridge's goroutine.
func (t *markTracker) echo(name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	timer, ok := t.outstanding[name]
	if !ok {
		log.Printf("twilio: realtime: mark echo %q matches no outstanding mark; not delivered as a match", name)
		return
	}
	timer.Stop()
	delete(t.outstanding, name)
	t.deliver(MarkEcho{Name: name})
}

// expire is the bound firing: the carrier never echoed this mark. The record
// still goes to the consumer, carrying TimedOut, because the whole point of
// the bound is that a peer which does not honor the mark protocol must not
// leave the consumer waiting forever.
func (t *markTracker) expire(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	if _, ok := t.outstanding[name]; !ok {
		// Echoed while this timer was already running its func and waiting on
		// the mutex. Stop() cannot unwind that, so the check here is what
		// keeps one mark from being reported twice.
		return
	}
	delete(t.outstanding, name)
	log.Printf("twilio: realtime: carrier did not honor mark protocol for %q within its bound", name)
	t.deliver(MarkEcho{Name: name, TimedOut: true})
}

// stop ends the tracker with the call. Every outstanding bound is cancelled and
// nothing is delivered afterwards: the consumer's call has returned, so a
// record arriving now describes a call that no longer exists — and on a channel
// the consumer may already be reusing for the next one.
func (t *markTracker) stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	for name, timer := range t.outstanding {
		timer.Stop()
		delete(t.outstanding, name)
	}
}

// deliver hands rec to the consumer without ever blocking, through the same
// sendOrDrop idiom every other consumer-facing seam on this path uses. Callers
// hold t.mu; sendOrDrop never blocks, so holding it across the send cannot
// deadlock.
func (t *markTracker) deliver(rec MarkEcho) {
	if t.echoCh == nil {
		return
	}
	sendOrDrop(t.echoCh, rec, &t.dropped, "mark echo")
}

// handleMarkRequest processes one receive from the mark-request select case,
// returning the channel HandleStreamRealtime's loop should keep selecting on
// next (nil once the consumer has closed it) and any error that should end the
// call. Mirrors handleClientEvent and handleVoiceUpdate — same extraction, same
// bound, same failure path — except that the frame goes to the CARRIER rather
// than the backend, and therefore through the sink.
//
// Through the sink is the whole point. carrierMediaSink.Media writes the
// carrier connection from Bridge.Run's read-loop goroutine while this runs on
// HandleStreamRealtime's select loop, so a mark written directly from here
// would be a second writer on that connection AND wrong ordering: it could
// land ahead of audio the bridge had not yet flushed, and the echo would then
// report a playout position that has not happened. Routing it through the
// sink's own write slot is what makes the mark land after the last media frame
// written before it, which is the property a consumer is buying.
//
// Deliberately does NOT reset the idle guard, for the reason handleClientEvent
// gives: a mark request is a consumer event, not backend activity, and a
// consumer marking steadily against a backend that has gone silent must not
// mask that silence.
//
// An EMPTY name is dropped rather than written. The name is the only thing that
// makes an echo matchable, so an unnamed mark would put a frame on the wire
// whose echo could never resolve to the request that caused it — the same
// shape of refusal handleVoiceUpdate makes for an empty voice.
func handleMarkRequest(ctx context.Context, sink *carrierMediaSink, ch <-chan string, name string, ok bool) (<-chan string, error) {
	if !ok {
		// The consumer closed its channel: return nil so the caller's select
		// case never fires again, rather than busy-looping on a closed channel
		// that is always ready to receive its zero value.
		return nil, nil
	}
	if name == "" {
		log.Printf("twilio: realtime: mark-request channel: refusing an empty mark name; its echo could never be matched")
		return ch, nil
	}

	// Bounded for the reason realtimeClientEventSendTimeout documents: this
	// write happens on the select loop that observes every way the call can
	// end, and the slot it queues behind is held by writes taken on the call's
	// own unbounded context.
	sendCtx, cancelSend := context.WithTimeout(ctx, realtimeClientEventSendTimeout)
	defer cancelSend()
	if err := sink.Mark(sendCtx, name); err != nil {
		log.Printf("twilio: realtime: mark send failed: %v", err)
		return ch, fmt.Errorf("twilio: realtime: mark send: %w", err)
	}
	return ch, nil
}

// muLawBytesInB64 reports how many μ-law bytes a base64 payload decodes to,
// without decoding it.
//
// Not decoding is deliberate and matches the rest of this path: carrier audio
// crosses this package as the base64 string it arrived as precisely so no
// frame is decoded and re-encoded, and the playout clock needs only the LENGTH.
// Each unpadded base64 character carries 6 bits, so the byte count is the
// whole-byte part of that — exact for both the padded form Twilio and the
// backend use and the unpadded form.
func muLawBytesInB64(payloadB64 string) int {
	n := len(payloadB64)
	for n > 0 && payloadB64[n-1] == '=' {
		n--
	}
	return n * 6 / 8
}
