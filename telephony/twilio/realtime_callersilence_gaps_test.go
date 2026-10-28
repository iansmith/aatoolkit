package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// Unit tests for the caller-silence bound's pieces (AATK-163), driven directly
// rather than through a call: each case below is an interleaving a call can
// produce only by a race, or a property no wire capture can see.

// turnEndedEvent is the response.done that ends an agent turn without a
// function call, as the drain observes it.
func turnEndedEvent() ServerEvent {
	return ServerEvent{Type: realtime.EventResponseDone, Raw: json.RawMessage(`{"type":"response.done"}`)}
}

// playedTurn takes s through one agent turn up to the playout echo: audio
// written, the turn ended, the mark requested and claimed, its echo reported.
// It returns the generation the claim captured.
func playedTurn(t *testing.T, s *callerSilence) uint64 {
	t.Helper()
	s.agentAudio()
	s.observe(turnEndedEvent())
	tag, ok := s.takeRequest()
	if !ok {
		t.Fatal("a turn-ending response.done after agent audio must request a playout mark")
	}
	gen, ok := s.claim(tag)
	if !ok {
		t.Fatal("a playout mark requested after agent audio, with no caller speech since, must be claimed")
	}
	s.played(gen)
	return gen
}

// TestCallerSilence_ASpeechStartTheDrainHasNotSeenVoidsTheMark pins the drain
// lag. The sink counts a speech start the moment Bridge.Run hands it the clear;
// the drain can still be behind it, having asked for a playout mark on the
// strength of a turn ending it saw first. That mark would be written behind the
// caller's speech and its echo would start a count while they talk, so the
// claim inside the write slot must refuse it.
//
// Mutation: delete the starts == tag check from callerSilence.claim.
func TestCallerSilence_ASpeechStartTheDrainHasNotSeenVoidsTheMark(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spoke     bool
		wantClaim bool
	}{
		{"no speech in between", false, true},
		{"a speech start the drain has not seen", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCallerSilence(time.Hour)
			t.Cleanup(s.stop)

			s.agentAudio()
			s.observe(turnEndedEvent())
			tag, ok := s.takeRequest()
			if !ok {
				t.Fatal("a turn-ending response.done after agent audio must request a playout mark")
			}
			if tc.spoke {
				s.callerSpoke()
			}
			if _, ok := s.claim(tag); ok != tc.wantClaim {
				t.Fatalf("claim = %v, want %v", ok, tc.wantClaim)
			}
		})
	}
}

// TestCallerSilence_AStaleFireIsIgnoredAndEndRefusesAfterSpeech pins the two
// races between the countdown's timer and the things that stop it.
func TestCallerSilence_AStaleFireIsIgnoredAndEndRefusesAfterSpeech(t *testing.T) {
	// A count was started, then agent audio stopped it and a later turn's echo
	// started a fresh one. The first count's timer had already begun running
	// its function — Stop cannot unwind that — and fires now. It must not end
	// the fresh count early. Mutation: delete the gen check from
	// callerSilence.fire.
	t.Run("stale fire", func(t *testing.T) {
		s := newCallerSilence(time.Hour)
		t.Cleanup(s.stop)

		first := playedTurn(t, s)
		second := playedTurn(t, s)
		if first == second {
			t.Fatalf("two turns must claim different generations, both got %d", first)
		}

		s.fire(first)
		select {
		case <-s.expired():
			t.Fatal("a fire from a count that has since been replaced must not signal expiry")
		case <-time.After(50 * time.Millisecond):
		}
	})

	// The count ran out and signalled, and the caller spoke before the select
	// loop got to it. end must withdraw that ending rather than hang up on a
	// caller who has started talking. Mutations: delete the due reset from
	// stopTimerLocked; delete the due check from end.
	for _, tc := range []struct {
		name    string
		spoke   bool
		wantEnd bool
	}{
		{"expired, nothing since", false, true},
		{"expired, then the caller spoke", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCallerSilence(time.Millisecond)
			t.Cleanup(s.stop)

			playedTurn(t, s)
			select {
			case <-s.expired():
			case <-time.After(2 * time.Second):
				t.Fatal("a played turn's count must expire at its bound")
			}
			if tc.spoke {
				s.callerSpoke()
			}
			if got := s.end(); got != tc.wantEnd {
				t.Fatalf("end() = %v, want %v", got, tc.wantEnd)
			}
		})
	}
}

// TestMarkTracker_PlayoutMarkResolvesThroughItsCallback pins the playout mark's
// containment at the tracker. Its resolution goes to the engine's callback and
// nowhere else — not to the consumer's echo channel, and not to the farewell's
// wait, which is outstanding beside it here.
//
// Mutations: delete the played branch from echo; delete it from expire; delete
// the playoutArmed gate (suppress on the prefix alone). That stop never calls
// played is an insertion check: adding a played call to stop turns it red.
func TestMarkTracker_PlayoutMarkResolvesThroughItsCallback(t *testing.T) {
	const (
		sid      = "SSplayout"
		farewell = realtimeFarewellMarkPrefix + sid
		echoed   = realtimePlayoutMarkPrefix + sid + "-1"
		expired  = realtimePlayoutMarkPrefix + sid + "-2"
		pending  = realtimePlayoutMarkPrefix + sid + "-3"
	)
	buf := captureLog(t)

	echoes := make(chan MarkEcho, 8)
	tr := newMarkTracker(echoes, true)
	fw := tr.await(farewell)

	var echoedCalls, expiredCalls, stopCalls atomic.Int32
	tr.armPlayout(echoed, time.Hour, func() { echoedCalls.Add(1) })
	tr.echo(echoed)
	if n := echoedCalls.Load(); n != 1 {
		t.Fatalf("an echoed playout mark must call its callback once, called %d times", n)
	}

	tr.armPlayout(expired, time.Millisecond, func() { expiredCalls.Add(1) })
	waitFor(t, 2*time.Second, func() bool { return expiredCalls.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if n := expiredCalls.Load(); n != 1 {
		t.Fatalf("a playout mark whose bound fires must call its callback once, called %d times", n)
	}

	select {
	case rec := <-echoes:
		t.Fatalf("a playout mark must never reach the consumer's echo channel, got %+v", rec)
	default:
	}
	select {
	case <-fw:
		t.Fatal("a playout mark resolving must not release the farewell's wait")
	default:
	}

	// A late echo of the mark whose bound already fired matches nothing
	// outstanding, and is not logged: it is the engine's, not the consumer's.
	tr.echo(expired)
	if strings.Contains(buf.String(), "matches no outstanding mark") {
		t.Fatalf("a late echo of an engine playout mark must not be logged as unmatched\nlog: %q", buf.String())
	}

	tr.arm(farewell, time.Hour)
	tr.echo(farewell)
	select {
	case <-fw:
	default:
		t.Fatal("the farewell's own echo must release its wait")
	}

	tr.armPlayout(pending, time.Hour, func() { stopCalls.Add(1) })
	tr.stop()
	if n := stopCalls.Load(); n != 0 {
		t.Fatalf("stopping the tracker must never report a playout mark as played, called %d times", n)
	}

	// A tracker that never armed a playout mark logs an unmatched echo of that
	// shape as it logs any other: the suppression is the engine's, for the
	// engine's own marks, and only where it wrote some.
	other := newMarkTracker(nil, true)
	t.Cleanup(other.stop)
	other.echo(realtimePlayoutMarkPrefix + "x")
	if !strings.Contains(buf.String(), "matches no outstanding mark") {
		t.Fatalf("a tracker with no playout marks must still log an unmatched echo\nlog: %q", buf.String())
	}
}

// TestBoundedSend_KeepsEachSendersLogAndErrorBytes pins the bytes the four
// bounded senders log and return when a write fails, now that they share one
// definition of that shape. Each sender is driven through its own call site,
// so a label passed wrongly is caught as surely as a format string that drifts.
func TestBoundedSend_KeepsEachSendersLogAndErrorBytes(t *testing.T) {
	boom := errors.New("boom")

	// check asserts the error and the log line carry exactly what's label
	// around the underlying write error.
	check := func(t *testing.T, buf *syncBuffer, err error, what string) {
		t.Helper()
		if err == nil {
			t.Fatalf("a failed %s write must return an error", what)
		}
		inner := errors.Unwrap(err)
		if inner == nil {
			t.Fatalf("the %s error must wrap the write error, got %v", what, err)
		}
		if want := "twilio: realtime: " + what + " send: " + inner.Error(); err.Error() != want {
			t.Fatalf("error:\n got %q\nwant %q", err.Error(), want)
		}
		if line := "twilio: realtime: " + what + " send failed: " + inner.Error() + "\n"; !strings.Contains(buf.String(), line) {
			t.Fatalf("log must carry %q\nlog: %q", line, buf.String())
		}
	}

	// backendClient dials the fake backend through a connection whose writes
	// fail once the handshake is done.
	backendClient := func(t *testing.T) *realtime.Client {
		t.Helper()
		be := newFakeRealtimeBackend(t)
		hc, failWrites := faultyHTTPClient()
		client, err := realtime.Dial(context.Background(), be.url(), realtime.WithHTTPClient(hc))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
		failWrites.Store(true)
		return client
	}

	t.Run("client event", func(t *testing.T) {
		buf := captureLog(t)
		_, err := handleClientEvent(context.Background(), backendClient(t), nil, clientEventRaw(1), true)
		check(t, buf, err, "client event")
	})
	t.Run("voice update", func(t *testing.T) {
		buf := captureLog(t)
		_, err := handleVoiceUpdate(context.Background(), backendClient(t), nil, "cedar", true)
		check(t, buf, err, "voice update")
	})
	t.Run("mark", func(t *testing.T) {
		buf := captureLog(t)
		sink := newCarrierMediaSink(&failingWSWriter{err: boom}, "SSmark", nil, nil, nil, nil)
		_, err := handleMarkRequest(context.Background(), sink, nil, "m1", true)
		check(t, buf, err, "mark")
		if err.Error() != "twilio: realtime: mark send: boom" {
			t.Fatalf("mark error = %q", err.Error())
		}
	})
	t.Run("playout mark", func(t *testing.T) {
		buf := captureLog(t)
		s := newCallerSilence(time.Hour)
		t.Cleanup(s.stop)
		sink := newCarrierMediaSink(&failingWSWriter{err: boom}, "SSplayout", nil, nil, nil, s)
		s.agentAudio()
		s.observe(turnEndedEvent())
		err := handlePlayoutMark(context.Background(), sink, s)
		check(t, buf, err, "playout mark")
		if err.Error() != "twilio: realtime: playout mark send: boom" {
			t.Fatalf("playout mark error = %q", err.Error())
		}
	})
}

// TestNewCallerSilence_ZeroAndNegativeAreNilAndInert pins the off switch at the
// type: no bound is no object, and every method on that nil is a no-op, so a
// call that does not set the option runs no timer, writes no mark and selects
// on two nil channels that never fire.
//
// Mutation: delete the after <= 0 nil return, or any method's nil guard.
func TestNewCallerSilence_ZeroAndNegativeAreNilAndInert(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		s := newCallerSilence(d)
		if s != nil {
			t.Fatalf("newCallerSilence(%v) must be nil", d)
		}
		s.callerSpoke()
		s.agentAudio()
		s.observe(turnEndedEvent())
		if _, ok := s.takeRequest(); ok {
			t.Fatal("a nil callerSilence must request no mark")
		}
		_ = s.nextMarkName("SSnil")
		if _, ok := s.claim(0); ok {
			t.Fatal("a nil callerSilence must claim no mark")
		}
		s.played(0)
		s.fire(0)
		if s.end() {
			t.Fatal("a nil callerSilence must never end a call")
		}
		s.stop()
		if s.markDue() != nil || s.expired() != nil {
			t.Fatal("a nil callerSilence must select on nil channels")
		}
	}
}
