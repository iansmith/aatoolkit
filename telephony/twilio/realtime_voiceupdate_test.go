package twilio

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// Voice-update-channel tests for the realtime path (AATK-104, "change the
// backend's output voice mid-call, without opening the session-config
// passthrough").
//
// Before this ticket WithVoice could set the backend's output voice once, at
// dial, and never again; AATK-93 closed the obvious alternative by refusing a
// session.update on the client-event channel. That refusal is right and stays
// — this channel is its other half. The consumer writes a voice id (a string,
// never event bytes) and the engine builds the minimal session.update itself,
// so turn detection, instructions and tools stay unreachable from here.
//
// These tests reuse realtime_clientevents_test.go's fixtures (receivedEvents,
// waitForRawEvent) and realtime_wiring_test.go's harness rather than growing a
// second copy of either.

// voiceUpdateFrames returns every session.update the backend received on the
// live connection, in arrival order. The fake backend records the dial
// handshake into b.handshakes separately from b.received
// (realtime_wiring_test.go:100-109), so nothing here can be satisfied by the
// handshake's own session.update — which is exactly what makes the "reaches
// the backend" clause below discriminating.
func (b *fakeRealtimeBackend) voiceUpdateFrames() []json.RawMessage {
	var out []json.RawMessage
	for _, raw := range b.receivedEvents() {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Type == "session.update" {
			out = append(out, raw)
		}
	}
	return out
}

// waitForVoiceUpdate waits until the backend has received at least one
// session.update on the live connection and returns the first one.
func waitForVoiceUpdate(t *testing.T, be *fakeRealtimeBackend, d time.Duration) json.RawMessage {
	t.Helper()
	deadline := time.After(d)
	for {
		if frames := be.voiceUpdateFrames(); len(frames) > 0 {
			return frames[0]
		}
		select {
		case <-deadline:
			t.Fatal("backend never received a session.update on the live connection")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// jsonKeyPaths walks a decoded JSON document and returns every key path in it,
// dotted, sorted. It is how the "carries the voice and nothing else"
// assertions below pin the key SET rather than a serialization: key order is
// not part of the contract, so a string compare would pin something the
// contract does not say, and a substring check would miss exactly the defect
// worth catching (a format key nested two levels down).
func jsonKeyPaths(v any, prefix string, out *[]string) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for k, child := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		*out = append(*out, path)
		jsonKeyPaths(child, path, out)
	}
}

func keyPathsOf(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode session.update: %v (raw: %s)", err, raw)
	}
	var paths []string
	jsonKeyPaths(doc, "", &paths)
	sort.Strings(paths)
	return paths
}

// --- RED #1: the voice reaches the backend, and the call continues ---------

// TestVoiceUpdateChan_VoiceReachesBackendAndCallContinues is the ticket's
// first RED case: a voice id written to the voice-update channel must reach
// the backend as a session.update carrying that voice, AND the call must
// continue afterwards — a client event written next still arrives.
//
// The conjunction is the assertion. "The call continues" alone is green at
// base (nothing ends a call today for a voice update that cannot be sent),
// so it cannot be the red test on its own; "a session.update arrives" alone
// would be satisfied by an implementation that sends one and then wedges or
// ends the call.
//
// slopstop:test contract
func TestVoiceUpdateChan_VoiceReachesBackendAndCallContinues(t *testing.T) {
	voices := make(chan string, testChanBuffer)
	events := make(chan json.RawMessage, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithVoiceUpdateChan(voices),
		WithClientEventChan(events),
	))
	waitBackendReady(t, be, h)

	voices <- "cedar"
	frame := waitForVoiceUpdate(t, be, 5*time.Second)

	var got struct {
		Type    string `json:"type"`
		Session struct {
			Type  string `json:"type"`
			Audio struct {
				Output struct {
					Voice string `json:"voice"`
				} `json:"output"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(frame, &got); err != nil {
		t.Fatalf("decode session.update: %v (raw: %s)", err, frame)
	}
	if got.Type != "session.update" {
		t.Fatalf("frame type = %q, want session.update (raw: %s)", got.Type, frame)
	}
	if got.Session.Type != "realtime" {
		t.Fatalf("session.type = %q, want realtime (raw: %s)", got.Session.Type, frame)
	}
	if got.Session.Audio.Output.Voice != "cedar" {
		t.Fatalf("session.audio.output.voice = %q, want cedar (raw: %s)",
			got.Session.Audio.Output.Voice, frame)
	}

	// The call continues: a client event written afterwards still arrives.
	events <- clientEventRaw(300)
	waitForClientEvent(t, be, 300, 5*time.Second)
}

// --- RED #2: the update carries the voice and nothing else ----------------

// TestVoiceUpdateChan_UpdateCarriesVoiceAndNothingElse is the ticket's second
// RED case, and the artifact that separates the correct implementation from
// one that re-uses newSessionUpdate. audioChannel.Format has no omitempty
// (telephony/realtime/events.go:45), and the backend deep-merges exactly the
// fields the update actually sent — so a re-use would send
// audio.input.format.type:"" and audio.output.format.type:"", overwriting the
// negotiated G.711 mu-law format on BOTH directions, mid-call. That defect
// passes RED #1 untouched.
//
// The assertion is a key-set walk of the decoded document, not a substring
// check: "no format key anywhere" is a statement about the whole tree.
//
// slopstop:test contract
func TestVoiceUpdateChan_UpdateCarriesVoiceAndNothingElse(t *testing.T) {
	voices := make(chan string, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithVoiceUpdateChan(voices)))
	waitBackendReady(t, be, h)

	voices <- "marin"
	frame := waitForVoiceUpdate(t, be, 5*time.Second)

	want := []string{
		"session",
		"session.audio",
		"session.audio.output",
		"session.audio.output.voice",
		"session.type",
		"type",
	}
	got := keyPathsOf(t, frame)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session.update key set =\n  %v\nwant\n  %v\n(raw: %s)", got, want, frame)
	}
}

// --- regression guard: no option, no extra frames -------------------------

// TestVoiceUpdateChan_NoOptionSendsNoSessionUpdate pins that a call supplying
// no voice-update option behaves exactly as it does today: the engine never
// puts a session.update on the live connection, whatever else the call does.
// Not a red test — it is green at base and must stay green.
//
// slopstop:test regression — guards: "with no voice-update option supplied, the frames the backend receives are what it receives today"
func TestVoiceUpdateChan_NoOptionSendsNoSessionUpdate(t *testing.T) {
	events := make(chan json.RawMessage, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithClientEventChan(events)))
	waitBackendReady(t, be, h)

	events <- clientEventRaw(310)
	waitForClientEvent(t, be, 310, 5*time.Second)

	if frames := be.voiceUpdateFrames(); len(frames) != 0 {
		t.Fatalf("no voice-update option was supplied, but the backend received %d session.update frame(s): %s",
			len(frames), frames[0])
	}
}

// --- an empty string is ignored -------------------------------------------

// TestVoiceUpdateChan_EmptyStringSendsNothing pins observable behaviour 5.
// An empty voice is what WithVoice means by "no voice" (events.go:47-50,
// omitempty), so sending one mid-call would be asking the backend to unset a
// field rather than to change it, and this package has no way to say what
// unsetting a voice mid-call means.
//
// The real id written afterwards is what makes this discriminating: without
// it, "exactly one session.update" would also hold for an implementation that
// dropped every voice update.
//
// slopstop:test contract
func TestVoiceUpdateChan_EmptyStringSendsNothing(t *testing.T) {
	voices := make(chan string, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithVoiceUpdateChan(voices)))
	waitBackendReady(t, be, h)

	voices <- ""
	voices <- "juniper"
	frame := waitForVoiceUpdate(t, be, 5*time.Second)

	if got := voiceOf(t, frame); got != "juniper" {
		t.Fatalf("first session.update carries voice %q, want juniper — the empty string must not have been sent (raw: %s)",
			got, frame)
	}

	// Give any second frame time to arrive before counting.
	time.Sleep(200 * time.Millisecond)
	if frames := be.voiceUpdateFrames(); len(frames) != 1 {
		t.Fatalf("backend received %d session.update frame(s), want exactly 1: %v", len(frames), frames)
	}
}

// --- idle guard ------------------------------------------------------------

// TestVoiceUpdateChan_DoesNotResetIdleTimer pins observable behaviour 4, the
// same rule AATK-93 states for a refused session.update: a voice update is a
// consumer event, not backend activity, so a chatty consumer against a silent
// backend must not mask that silence.
//
// The positive proof comes first: without it, "the idle timer still fires"
// holds trivially against an implementation that never reads the channel.
//
// slopstop:test non-interference — paired: asserts one voice update actually reaches the backend before checking that steady voice traffic against a silent backend still ends the call with "idle timeout"
func TestVoiceUpdateChan_DoesNotResetIdleTimer(t *testing.T) {
	voices := make(chan string, 1)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithVoiceUpdateChan(voices),
		WithIdleTimeout(idleGapTimeout),
	))
	waitBackendReady(t, be, h)

	// Positive: the channel is genuinely wired.
	voices <- "cedar"
	waitForVoiceUpdate(t, be, 5*time.Second)

	// Negative: keep the consumer sending steadily while the backend stays
	// silent. The call must still end on the idle timer.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		ticker := time.NewTicker(idleGapTimeout / 10)
		defer ticker.Stop()
		n := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				select {
				case voices <- fmt.Sprintf("voice-%d", n):
				default:
				}
				n++
			}
		}
	}()

	err := h.waitDone(idleGapTimeout + 2*time.Second)
	if err == nil {
		t.Fatal("a silent backend must still end the call even while the consumer sends voice updates steadily")
	}
	if !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("error must name the idle timeout, got: %v", err)
	}
}

// --- closed consumer channel ----------------------------------------------

// TestVoiceUpdateChan_ClosedChannelLeavesCallRunning pins observable
// behaviour 8: closing the channel stops the case firing rather than ending
// the call or busy-looping on a ready receive of the zero value. Exactly what
// a closed client-event channel does today.
//
// slopstop:test contract
func TestVoiceUpdateChan_ClosedChannelLeavesCallRunning(t *testing.T) {
	voices := make(chan string, testChanBuffer)
	events := make(chan json.RawMessage, testChanBuffer)

	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(),
		WithVoiceUpdateChan(voices),
		WithClientEventChan(events),
	))
	waitBackendReady(t, be, h)

	voices <- "cedar"
	waitForVoiceUpdate(t, be, 5*time.Second)

	close(voices)
	time.Sleep(200 * time.Millisecond)
	assertStillRunning(t, h, "closing the voice-update channel must not end the call")

	// And nothing further is forwarded: the zero value the closed channel
	// yields must never reach the backend as a session.update.
	events <- clientEventRaw(320)
	waitForClientEvent(t, be, 320, 5*time.Second)
	if frames := be.voiceUpdateFrames(); len(frames) != 1 {
		t.Fatalf("backend received %d session.update frame(s) after the channel closed, want exactly 1: %v",
			len(frames), frames)
	}
}

// --- WithVoiceUpdateChanFor varies per call -------------------------------

// TestVoiceUpdateChanFor_VariesPerCallThroughOneHandler mirrors
// TestClientEventChanFor_VariesPerCallThroughOneHandler: two calls through the
// SAME handler must route to two different channels, which is the assertion
// WithVoiceUpdateChan's constant channel cannot satisfy.
//
// slopstop:test contract
func TestVoiceUpdateChanFor_VariesPerCallThroughOneHandler(t *testing.T) {
	be := newFakeRealtimeBackend(t)

	calls := 0
	handler := NewStreamHandler(be.url(), WithVoiceUpdateChanFor(func(start Frame) <-chan string {
		calls++
		ch := make(chan string, testChanBuffer)
		if calls == 1 {
			ch <- "cedar"
		} else {
			ch <- "marin"
		}
		return ch
	}))

	for i, want := range []string{"cedar", "marin"} {
		h := newRealtimeHarnessWith(t, handler)
		waitBackendReady(t, be, h)
		waitForVoiceUpdateCount(t, be, i+1, 5*time.Second)

		frames := be.voiceUpdateFrames()
		if got := voiceOf(t, frames[i]); got != want {
			t.Fatalf("call %d: voice = %q, want %q", i+1, got, want)
		}

		h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
		_ = h.waitDone(5 * time.Second)
	}
}

// voiceOf decodes session.audio.output.voice out of one session.update frame.
func voiceOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var got struct {
		Session struct {
			Audio struct {
				Output struct {
					Voice string `json:"voice"`
				} `json:"output"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode session.update: %v (raw: %s)", err, raw)
	}
	return got.Session.Audio.Output.Voice
}

// waitForVoiceUpdateCount waits until the backend has received at least n
// session.update frames on live connections.
func waitForVoiceUpdateCount(t *testing.T, be *fakeRealtimeBackend, n int, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		if len(be.voiceUpdateFrames()) >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("backend received %d session.update frame(s), want at least %d",
				len(be.voiceUpdateFrames()), n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// --- closed channel: the CASE is retired, measured not inferred -----------

// TestVoiceUpdateChan_ClosedChannelDoesNotBusyLoop is the measured half of
// observable behaviour 8, and the sibling of
// TestClientEventChan_ClosedChannelDoesNotBusyLoop. Liveness alone cannot
// see this defect: an implementation that returns the channel unchanged on a
// closed receive spins on the zero value forever, and every zero value it
// reads is the empty string the engine already drops — so no extra frame
// reaches the backend, the call stays up, and every liveness assertion in
// this file still passes. Mutation-verified: replacing handleVoiceUpdate's
// `return nil, nil` with `return ch, nil` left the whole telephony suite
// green before this test existed.
//
// Measured differentially against the same window with no voice-update
// option supplied, exactly as the client-event version does, with generous
// headroom: a genuine busy loop burns an entire core continuously, orders of
// magnitude more than idle call machinery.
//
// slopstop:test regression — guards: "a closed voice-update channel retires its select case rather than busy-looping on the zero value"
func TestVoiceUpdateChan_ClosedChannelDoesNotBusyLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("getrusage is not available on windows")
	}

	const window = 300 * time.Millisecond

	measure := func(withClosedChan bool) time.Duration {
		be := newFakeRealtimeBackend(t)
		var h *realtimeHarness
		if withClosedChan {
			ch := make(chan string)
			close(ch)
			h = newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithVoiceUpdateChan(ch)))
		} else {
			h = newRealtimeHarness(t, be.url())
		}
		waitBackendReady(t, be, h)

		var before, after syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
			t.Fatalf("getrusage: %v", err)
		}
		time.Sleep(window)
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
			t.Fatalf("getrusage: %v", err)
		}

		h.sendRaw([]byte(`{"event":"stop","streamSid":"` + h.streamSID + `"}`))
		_ = h.waitDone(5 * time.Second)

		userDelta := time.Duration(after.Utime.Sec-before.Utime.Sec)*time.Second +
			time.Duration(after.Utime.Usec-before.Utime.Usec)*time.Microsecond
		sysDelta := time.Duration(after.Stime.Sec-before.Stime.Sec)*time.Second +
			time.Duration(after.Stime.Usec-before.Stime.Usec)*time.Microsecond
		return userDelta + sysDelta
	}

	baseline := measure(false)
	withClosed := measure(true)

	if withClosed > baseline+50*time.Millisecond {
		t.Fatalf("a closed voice-update channel must not busy-loop: baseline CPU %v, with-closed-channel CPU %v (over a %v wall-clock window)",
			baseline, withClosed, window)
	}
}

// --- send failure ---------------------------------------------------------

// TestVoiceUpdateChan_SendFailureEndsCallWithLoggedError pins observable
// behaviour 6, the sibling of
// TestClientEventChan_SendFailureEndsCallWithLoggedError: this transport's
// writer poisons itself after one write error, so a failed voice-update send
// is never transient. It is logged rather than silently swallowed, and the
// call ends with a non-nil error rather than continuing against a connection
// whose write half is dead forever.
//
// Mutation-verified: swallowing the send error (`return ch, nil` in place of
// the wrapped return) left the whole telephony suite green before this test
// existed.
//
// slopstop:test contract
func TestVoiceUpdateChan_SendFailureEndsCallWithLoggedError(t *testing.T) {
	var buf syncBuffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	hc, failWrites := faultyHTTPClient()
	origDial := dialRealtime
	dialRealtime = func(ctx context.Context, url string, opts ...realtime.DialOption) (*realtime.Client, error) {
		return origDial(ctx, url, append(opts, realtime.WithHTTPClient(hc))...)
	}
	defer func() { dialRealtime = origDial }()

	voices := make(chan string, 1)
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithVoiceUpdateChan(voices)))
	waitBackendReady(t, be, h)

	failWrites.Store(true)
	voices <- "cedar"

	err := h.waitDone(5 * time.Second)
	if err == nil {
		t.Fatal("a voice-update send failure must end the call with a non-nil error, not leave it running")
	}
	// Attribution, not merely "some error": the transport's write error reads
	// identically whichever writer produced it.
	if !strings.Contains(err.Error(), "twilio: realtime: voice update send") {
		t.Fatalf("the call must end naming the voice-update send in its error, got: %v", err)
	}
	if !strings.Contains(buf.String(), "injected write failure") {
		t.Fatalf("a failed voice-update send must be logged before the call ends; log output: %q", buf.String())
	}
}

// --- the send bound -------------------------------------------------------

// TestVoiceUpdateChan_SendThatNeverCompletesEndsCallOnItsBound pins
// observable behaviour 7, the sibling of
// TestClientEventChan_SendThatNeverCompletesEndsCallOnItsBound. The
// voice-update write runs on HandleStreamRealtime's own select loop, so a
// write that never returns parks the loop and silences backendDone,
// carrierDone and the idle timer along with it. waitDone failing on timeout
// is that hang; a non-nil error inside the window is
// realtimeClientEventSendTimeout working.
//
// Mutation-verified: replacing the bounded context with a plain
// context.WithCancel(ctx) left the whole telephony suite green before this
// test existed.
//
// slopstop:test regression — guards: "a voice-update write that never completes is bounded, so it cannot park HandleStreamRealtime's select loop, and the call ends naming the voice-update send"
func TestVoiceUpdateChan_SendThatNeverCompletesEndsCallOnItsBound(t *testing.T) {
	hc, stall := stalledHTTPClient()
	origDial := dialRealtime
	dialRealtime = func(ctx context.Context, url string, opts ...realtime.DialOption) (*realtime.Client, error) {
		return origDial(ctx, url, append(opts, realtime.WithHTTPClient(hc))...)
	}
	defer func() { dialRealtime = origDial }()

	voices := make(chan string, 1)
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithVoiceUpdateChan(voices)))
	waitBackendReady(t, be, h)

	stall.Store(true)
	voices <- "cedar"

	// Generous slack over the bound itself: the assertion is that the call
	// ends at all, not that it ends to the millisecond.
	err := h.waitDone(realtimeClientEventSendTimeout + 5*time.Second)
	if err == nil {
		t.Fatal("a voice-update write that never completes must end the call with a non-nil error on its own bound")
	}
	if !strings.Contains(err.Error(), "twilio: realtime: voice update send") {
		t.Fatalf("the call must end naming the voice-update send in its error, got: %v", err)
	}
}
