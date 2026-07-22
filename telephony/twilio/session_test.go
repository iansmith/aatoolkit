package twilio

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
)

// vadCycleLen is the number of inference windows a fake VAD needs to script
// one full speech -> end-of-utterance boundary through the real (unmodified)
// VAD machinery: 1 speech-triggering window, then enough silence windows to
// cross defaultVADConfig's EndSilenceMS.
//
// Derived, not literal: the silence count is a function of a VAD default, and
// hardcoding it means a tuning change surfaces here as "timeout waiting for
// first end-of-utterance" -- a failure that names the symptom in a package
// that has nothing to do with the cause.
var vadCycleLen = int32(1 + telephony.EndSilenceWindows())

// fakeVAD is a deterministic telephony.VADDetector: it ignores window
// content entirely and scripts a probability purely by call count, cycling
// every vadCycleLen windows so repeated utterance pushes each produce their
// own speech/end-of-utterance boundary.
type fakeVAD struct {
	calls  int32
	resets int32
}

func (f *fakeVAD) Detect(window []float32) (float32, error) {
	idx := atomic.AddInt32(&f.calls, 1) - 1
	if idx%vadCycleLen == 0 {
		return 0.9, nil // >= SpeechThresh (0.5)
	}
	return 0.1, nil // < SilenceThresh (0.35)
}

func (f *fakeVAD) Reset() { atomic.AddInt32(&f.resets, 1) }

func (f *fakeVAD) resetCount() int32 { return atomic.LoadInt32(&f.resets) }

// recordingTurnSink is a telephony.TurnSink that counts and signals each
// boundary so tests can observe media reaching the session without racing
// on the internal channels.
type recordingTurnSink struct {
	mu              sync.Mutex
	speechStarts    int
	endOfUtterances int
	speechCh        chan struct{}
	eouCh           chan struct{}
}

func newRecordingTurnSink() *recordingTurnSink {
	return &recordingTurnSink{
		speechCh: make(chan struct{}, 16),
		eouCh:    make(chan struct{}, 16),
	}
}

func (r *recordingTurnSink) OnSpeechStart() {
	r.mu.Lock()
	r.speechStarts++
	r.mu.Unlock()
	r.speechCh <- struct{}{}
}

func (r *recordingTurnSink) OnEndOfUtterance() {
	r.mu.Lock()
	r.endOfUtterances++
	r.mu.Unlock()
	r.eouCh <- struct{}{}
}

func (r *recordingTurnSink) OnTurnComplete(string, telephony.TurnTrigger) {}

func (r *recordingTurnSink) eouCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.endOfUtterances
}

// harness wires an httptest server + real websocket client around
// handleStream via an injected newSession factory, giving tests a fake VAD,
// a recording TurnSink, and a channel signaling when the handler returns.
type harness struct {
	t    *testing.T
	conn *websocket.Conn
	vad  *fakeVAD
	sink *recordingTurnSink
	done chan error
	sess *telephony.Session
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		vad:  &fakeVAD{},
		sink: newRecordingTurnSink(),
		done: make(chan error, 1),
	}

	newSession := func(ctx context.Context, callSID string, opts ...telephony.SessionOption) *telephony.Session {
		opts = append([]telephony.SessionOption{
			telephony.WithVADFactory(func() (telephony.VADDetector, error) { return h.vad, nil }),
			telephony.WithTurnSink(h.sink),
		}, opts...)
		h.sess = telephony.NewSession(ctx, callSID, opts...)
		return h.sess
	}

	srv := &Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start Frame) error {
			err := handleStream(ctx, conn, start, testStreamStartedAt, newSession)
			h.done <- err
			return err
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(srv.ServeStreams))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	h.conn = conn

	msg, err := EncodeStart("SS"+t.Name(), "CA"+t.Name(), "AC"+t.Name(), 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		t.Fatalf("write start: %v", err)
	}

	return h
}

func (h *harness) sendRaw(msg []byte) {
	h.t.Helper()
	if err := h.conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		h.t.Fatalf("write: %v", err)
	}
}

func (h *harness) sendMedia(payload []byte) {
	h.t.Helper()
	msg, err := EncodeMedia("SS"+h.t.Name(), payload)
	if err != nil {
		h.t.Fatalf("EncodeMedia: %v", err)
	}
	h.sendRaw(msg)
}

// utterancePayload is enough mu-law bytes (vadCycleLen windows of 256
// samples) to drive the fake VAD through exactly one speech ->
// end-of-utterance boundary. Content is irrelevant: fakeVAD ignores it.
func utterancePayload() []byte {
	return make([]byte, 256*vadCycleLen)
}

func waitForResets(t *testing.T, v *fakeVAD, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v.resetCount() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("VAD Reset() called %d times, want %d (session Close() not observed)", v.resetCount(), want)
}

// TestDefaultHandleStream_PumpsMediaToSession verifies that an inbound media
// frame's mu-law payload reaches the injected session's Call.In and is
// processed by the real VAD machinery: the fake VAD's scripted speech window
// must produce an OnSpeechStart on the recording TurnSink.
func TestDefaultHandleStream_PumpsMediaToSession(t *testing.T) {
	h := newHarness(t)
	h.sendMedia(utterancePayload())

	select {
	case <-h.sink.speechCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnSpeechStart — media frame did not reach the session's Call.In")
	}
}

// TestDefaultHandleStream_ClosesSessionOnStop verifies a stop frame closes
// the session exactly once, the handler returns cleanly, and -- critically --
// the session actually reached StateClosed via the transition table (not
// merely torn down by Close()'s ctx cancellation racing ahead of the stop
// ControlEvent's async delivery through the demux).
func TestDefaultHandleStream_ClosesSessionOnStop(t *testing.T) {
	h := newHarness(t)
	h.sendRaw([]byte(`{"event":"stop","streamSid":"SS` + t.Name() + `"}`))

	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return within 2s after stop frame")
	}

	if got := h.sess.State(); got != telephony.StateClosed {
		t.Fatalf("session state after stop frame: got %s, want Closed (transition table's stop handling was bypassed)", got)
	}

	waitForResets(t, h.vad, 1)
}

// TestDefaultHandleStream_ClosesSessionOnAbruptDisconnect verifies an
// unclean disconnect (read error) also closes the session exactly once and
// the handler returns.
func TestDefaultHandleStream_ClosesSessionOnAbruptDisconnect(t *testing.T) {
	h := newHarness(t)
	h.conn.CloseNow()

	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return within 2s after abrupt disconnect")
	}

	waitForResets(t, h.vad, 1)
}

// TestDefaultHandleStream_MultiUtteranceSameSession verifies two utterances
// on one connection are both processed on the same injected session without
// re-invoking the handler.
func TestDefaultHandleStream_MultiUtteranceSameSession(t *testing.T) {
	h := newHarness(t)

	h.sendMedia(utterancePayload())
	select {
	case <-h.sink.eouCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for first end-of-utterance")
	}

	h.sendMedia(utterancePayload())
	select {
	case <-h.sink.eouCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for second end-of-utterance on the same session")
	}

	if got := h.sink.eouCount(); got != 2 {
		t.Fatalf("OnEndOfUtterance called %d times, want 2", got)
	}
}

// TestDefaultHandleStream_SkipsUndecodableAndIgnoredFrames verifies a
// decode-failing frame (dtmf) is logged and skipped, and that a mark frame
// arriving outside AwaitingMarkEcho (routed to the control plane like any
// other mark, but unexpected in this state) is logged loudly and otherwise
// harmless -- neither frame is fatal, and the read loop keeps going.
func TestDefaultHandleStream_SkipsUndecodableAndIgnoredFrames(t *testing.T) {
	h := newHarness(t)

	// dtmf: unknown event type, DecodeFrame errors — must not be fatal.
	h.sendRaw([]byte(`{"event":"dtmf","streamSid":"SS` + t.Name() + `"}`))

	// mark: decodes fine and is now routed to the control plane (SOP-115
	// drift-check fix), but the session isn't in AwaitingMarkEcho here, so
	// it's an unexpected-input transition: logged loudly, state unchanged,
	// not fatal.
	markMsg, err := EncodeMark("SS"+t.Name(), "ignored")
	if err != nil {
		t.Fatalf("EncodeMark: %v", err)
	}
	h.sendRaw(markMsg)

	// A later media frame must still be processed — proves the loop continued.
	h.sendMedia(utterancePayload())
	select {
	case <-h.sink.speechCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not continue processing media after undecodable/ignored frames")
	}
}

// TestDefaultHandleStream_WiresRealOutputsAndClose verifies handleStream
// itself -- not just a test factory -- unconditionally supplies
// WithTwilioDataOutput, WithTwilioControlOutput, and WithCloseFunc to every
// session it constructs (SOP-125 code review: these were previously wired
// only by test-injected factories, so the whole termination flow was dead
// code on a real call). It drives the idle timer to fire over a real
// WebSocket connection and checks that the farewell clip and its mark
// actually arrive on the wire, and that once the mark-echo timeout elapses,
// the real connection is genuinely closed server-side.
func TestDefaultHandleStream_WiresRealOutputsAndClose(t *testing.T) {
	vad := &fakeVAD{}
	sink := newRecordingTurnSink()
	done := make(chan error, 1)

	newSession := func(ctx context.Context, callSID string, opts ...telephony.SessionOption) *telephony.Session {
		opts = append([]telephony.SessionOption{
			telephony.WithVADFactory(func() (telephony.VADDetector, error) { return vad, nil }),
			telephony.WithTurnSink(sink),
			telephony.WithMaxSilenceMS(50),
		}, opts...)
		return telephony.NewSession(ctx, callSID, opts...)
	}

	srv := &Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start Frame) error {
			err := handleStream(ctx, conn, start, testStreamStartedAt, newSession)
			done <- err
			return err
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(srv.ServeStreams))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	msg, err := EncodeStart("SS"+t.Name(), "CA"+t.Name(), "AC"+t.Name(), 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		t.Fatalf("write start: %v", err)
	}

	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var gotFarewell int
	var gotMark bool
	for !gotMark {
		_, raw, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("read from server: %v (farewell bytes so far=%d)", err, gotFarewell)
		}
		f, err := DecodeFrame(raw)
		if err != nil {
			t.Fatalf("decode server frame: %v", err)
		}
		switch f.Event {
		case EventMedia:
			gotFarewell += len(f.Payload)
		case EventMark:
			gotMark = true
		default:
			t.Fatalf("unexpected server frame event %q", f.Event)
		}
	}
	if gotFarewell == 0 {
		t.Fatal("handleStream never sent farewell audio over the real WebSocket -- WithTwilioDataOutput not wired")
	}

	// No mark echo is sent back: the mark-echo timeout must fire on its own
	// and close the real connection, proving WithCloseFunc is wired to the
	// live WebSocket rather than a test double.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleStream did not return after mark-echo timeout -- WithCloseFunc not wired to the real connection")
	}

	postCloseCtx, postCloseCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer postCloseCancel()
	if _, _, err := conn.Read(postCloseCtx); err == nil {
		t.Fatal("expected read error after server-side close (WithCloseFunc), got nil")
	}
}

// TestDefaultHandleStream_MarkEchoReceivedClosesPromptly is the regression
// test for the SOP-115 umbrella drift check: handleStream's read loop used
// to special-case only EventMedia/EventStop, so an inbound "mark" fell into
// a default branch that just logged and never reached demux.RouteFrame --
// the session's AwaitingMarkEcho control handler could only ever be
// exercised by tests that inject a ControlEvent directly (bypassing
// handleStream), never by a real mark echo arriving over the wire. This
// test sends a genuine EncodeMark("farewell") reply back to the server
// after the farewell clip and its mark arrive, and asserts the handler
// returns almost immediately -- proving the echo actually reached the
// session via demux.RouteFrame/pumpControlPlane/the transition table,
// rather than the session always falling through to the full
// MarkEchoTimeout and logging a false "peer did not honor mark protocol"
// warning on every real call.
func TestDefaultHandleStream_MarkEchoReceivedClosesPromptly(t *testing.T) {
	vad := &fakeVAD{}
	sink := newRecordingTurnSink()
	done := make(chan error, 1)

	newSession := func(ctx context.Context, callSID string, opts ...telephony.SessionOption) *telephony.Session {
		opts = append([]telephony.SessionOption{
			telephony.WithVADFactory(func() (telephony.VADDetector, error) { return vad, nil }),
			telephony.WithTurnSink(sink),
			telephony.WithMaxSilenceMS(50),
		}, opts...)
		return telephony.NewSession(ctx, callSID, opts...)
	}

	srv := &Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start Frame) error {
			err := handleStream(ctx, conn, start, testStreamStartedAt, newSession)
			done <- err
			return err
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(srv.ServeStreams))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	streamSID := "SS" + t.Name()
	msg, err := EncodeStart(streamSID, "CA"+t.Name(), "AC"+t.Name(), 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		t.Fatalf("write start: %v", err)
	}

	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var gotMark bool
	for !gotMark {
		_, raw, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("read from server: %v", err)
		}
		f, err := DecodeFrame(raw)
		if err != nil {
			t.Fatalf("decode server frame: %v", err)
		}
		switch f.Event {
		case EventMedia:
		case EventMark:
			gotMark = true
		default:
			t.Fatalf("unexpected server frame event %q", f.Event)
		}
	}

	markEcho, err := EncodeMark(streamSID, "farewell")
	if err != nil {
		t.Fatalf("EncodeMark: %v", err)
	}
	echoSentAt := time.Now()
	if err := conn.Write(context.Background(), websocket.MessageText, markEcho); err != nil {
		t.Fatalf("write mark echo: %v", err)
	}

	select {
	case <-done:
		// MarkEchoGraceMS alone is 500ms; a genuinely-routed echo should
		// close the session in a small fraction of that, not by coincidentally
		// racing the full derived timeout.
		if elapsed := time.Since(echoSentAt); elapsed >= 300*time.Millisecond {
			t.Fatalf("handleStream took %v to close after the mark echo -- looks like it fell through to the timeout instead of routing the echo", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleStream did not return after a real mark echo was sent -- mark frames are still not reaching the control plane")
	}
}

// TestWaitForClosedReturnsPromptly verifies that handleStream returns quickly
// after a stop frame, rather than waiting for the full stopDrainTimeout (500ms),
// thanks to the Closed() channel replacing the polling loop.
func TestWaitForClosedReturnsPromptly(t *testing.T) {
	done := make(chan error)
	var stopFrameReceivedTime time.Time

	srv := &Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start Frame) error {
			newSession := func(ctx context.Context, callSID string, opts ...telephony.SessionOption) *telephony.Session {
				allOpts := []telephony.SessionOption{
					telephony.WithVADFactory(func() (telephony.VADDetector, error) {
						return &fakeVAD{}, nil
					}),
					telephony.WithTurnSink(newRecordingTurnSink()),
				}
				allOpts = append(allOpts, opts...)
				return telephony.NewSession(ctx, callSID, allOpts...)
			}
			err := handleStream(ctx, conn, start, testStreamStartedAt, newSession)
			elapsed := time.Since(stopFrameReceivedTime)
			if elapsed < 100*time.Millisecond {
				done <- nil
			} else {
				done <- fmt.Errorf("handleStream took %v to return; expected < 100ms", elapsed)
			}
			return err
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(srv.ServeStreams))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	streamSID := "SS" + t.Name()
	msg, err := EncodeStart(streamSID, "CA"+t.Name(), "AC"+t.Name(), 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		t.Fatalf("write start: %v", err)
	}

	// Wait a moment for handleStream to start
	time.Sleep(10 * time.Millisecond)

	// Send a stop frame
	stopMsg, err := EncodeStop(streamSID, "CA"+t.Name(), "AC"+t.Name(), 0)
	if err != nil {
		t.Fatalf("EncodeStop: %v", err)
	}
	stopFrameReceivedTime = time.Now()
	if err := conn.Write(context.Background(), websocket.MessageText, stopMsg); err != nil {
		t.Fatalf("write stop: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleStream returned with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleStream did not return within timeout")
	}
}

// TestHandleStreamWithOpts verifies that HandleStreamWithOpts itself (not
// handleStream's lower-level factory seam) correctly threads its extraOpts
// into the session it constructs. It calls HandleStreamWithOpts directly
// with telephony.WithSTTInput/WithSTTOutput and drives a real utterance over
// the wire; the only way an STTRequest can reach sttIn is if
// HandleStreamWithOpts's extraOpts actually made it onto the constructed
// session -- a fake/deleted HandleStreamWithOpts would leave sttIn empty and
// fail this test.
func TestHandleStreamWithOpts(t *testing.T) {
	vad := &fakeVAD{}
	sttIn := telephony.NewBufferedChan[telephony.STTRequest](10)
	sttOut := telephony.NewBufferedChan[telephony.STTResult](10)

	done := make(chan error, 1)

	srv := &Server{
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start Frame) error {
			err := HandleStreamWithOpts(ctx, conn, start,
				telephony.WithVADFactory(func() (telephony.VADDetector, error) { return vad, nil }),
				telephony.WithSTTInput(sttIn),
				telephony.WithSTTOutput(sttOut),
			)
			done <- err
			return err
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(srv.ServeStreams))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	streamSID := "SS" + t.Name()
	callSID := "CA" + t.Name()
	msg, err := EncodeStart(streamSID, callSID, "AC"+t.Name(), 1)
	if err != nil {
		t.Fatalf("EncodeStart: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		t.Fatalf("write start: %v", err)
	}

	mediaMsg, err := EncodeMedia(streamSID, utterancePayload())
	if err != nil {
		t.Fatalf("EncodeMedia: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, mediaMsg); err != nil {
		t.Fatalf("write media: %v", err)
	}

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer reqCancel()
	req, err := sttIn.Recv(reqCtx)
	if err != nil {
		t.Fatalf("no STTRequest dispatched via sttIn within timeout -- HandleStreamWithOpts's extraOpts (WithSTTInput) did not reach the session: %v", err)
	}
	if req.SessionID != callSID {
		t.Fatalf("dispatched STTRequest.SessionID: got %q, want %q", req.SessionID, callSID)
	}

	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Logf("close: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleStream did not return")
	}
}

// AATK-15. handleStreamDrainCheck drives K media frames into a live handler,
// triggers teardown via cut, and asserts every pre-teardown payload survived to
// <streamSID>.in.ulaw in order. It is the structural companion to
// TestTap_WiredToDataPlane: where that test proves the tap is wired, this proves
// the stop→drain→close boundary loses nothing at teardown. Under the old
// ctx-cancel teardown a buffered frame could be abandoned in a 50/50 select
// between a ready <-ch and a ready <-ctx.Done(); the close/drain boundary makes
// capture complete and load-independent. See design/teardown-protocol.md.
func handleStreamDrainCheck(t *testing.T, cut func(*harness)) {
	t.Helper()

	// Distinct, non-zero payloads so the on-disk concatenation pins order, not
	// merely byte count. K=3.
	payloads := [][]byte{{0x11, 0x11, 0x11}, {0x22, 0x22, 0x22}, {0x33, 0x33, 0x33}}

	dir := t.TempDir()
	t.Setenv(tapDirEnv, dir)

	h := newHarness(t)
	for _, p := range payloads {
		h.sendMedia(p)
	}
	cut(h)

	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleStream did not return after teardown")
	}

	streamSID := "SS" + t.Name()
	got, err := os.ReadFile(inulawPath(dir, streamSID))
	if err != nil {
		t.Fatalf("no recording after teardown — a pre-teardown media frame was lost from the tap: %v", err)
	}

	var want []byte
	for _, p := range payloads {
		want = append(want, p...)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("recording = % x (%d bytes), want % x (%d bytes) — a pre-teardown media frame was dropped instead of drained before tap.Close",
			got, len(got), want, len(want))
	}
}

// TestHandleStreamDrainsDataBeforeTapClose covers the graceful stop-frame exit
// path: all K media frames delivered before the stop are drained to the tap.
func TestHandleStreamDrainsDataBeforeTapClose(t *testing.T) {
	handleStreamDrainCheck(t, func(h *harness) {
		h.sendRaw([]byte(`{"event":"stop","streamSid":"SS` + t.Name() + `"}`))
	})
}

// TestHandleStreamDrainsDataOnReadErrorPath covers the read-error exit path
// (conn closed abruptly, no stop frame): the frames already read into the data
// plane are still drained to the tap before close, so the same K payloads land
// on disk.
func TestHandleStreamDrainsDataOnReadErrorPath(t *testing.T) {
	handleStreamDrainCheck(t, func(h *harness) {
		h.conn.CloseNow()
	})
}
