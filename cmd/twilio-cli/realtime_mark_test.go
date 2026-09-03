package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/iansmith/aatoolkit/telephony"
	"github.com/iansmith/aatoolkit/telephony/twilio"
)

// End-to-end mark round trip on the realtime path (AATK-105, observable
// behavior 4: "twilio-cli, driven end to end, echoes a mark sent this way
// after its playout estimate — so the round trip is exercisable without
// Twilio").
//
// This is the only test in the suite that closes the whole loop: a real
// twilio-cli process dials a real HTTP webhook, opens a real Media Streams
// WebSocket, and its existing echoMark (dial.go) answers a mark the realtime
// engine wrote. Nothing here changes echoMark — the ticket is explicit that it
// already does the right thing.

// cliRealtimeBackend is the minimum realtime voice backend this end-to-end run
// needs: it completes the session handshake and then emits audio deltas on
// demand, so the engine has audio to write to the carrier and the CLI has
// playout to wait out before echoing.
//
// It is deliberately not a copy of telephony/twilio's fakeRealtimeBackend:
// that one lives in that package's test binary and records what the handshake
// negotiated, which is not what is under test here. What this needs is a
// handle to emit on, mid-run, from the test's own goroutine.
type cliRealtimeBackend struct {
	srv *httptest.Server

	mu   sync.Mutex
	conn *websocket.Conn
}

func newCLIRealtimeBackend(t *testing.T) *cliRealtimeBackend {
	t.Helper()
	b := &cliRealtimeBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()

		// The session.update handshake the engine opens with.
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		if err := writeJSONFrame(ctx, c, map[string]string{"type": "session.created"}); err != nil {
			return
		}

		b.mu.Lock()
		b.conn = c
		b.mu.Unlock()

		// Drain whatever the engine forwards until the call ends.
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func writeJSONFrame(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, data)
}

func (b *cliRealtimeBackend) url() string {
	return "ws" + strings.TrimPrefix(b.srv.URL, "http")
}

// ready reports whether the handshake has completed, which is the precondition
// emit needs.
func (b *cliRealtimeBackend) ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != nil
}

// emit writes one audio delta from the backend, now.
func (b *cliRealtimeBackend) emit(t *testing.T, deltaB64 string) {
	t.Helper()
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	if c == nil {
		t.Fatal("backend has no connection to emit on")
	}
	if err := writeJSONFrame(context.Background(), c, map[string]string{
		"type":  "response.output_audio.delta",
		"delta": deltaB64,
	}); err != nil {
		t.Fatalf("emit audio delta: %v", err)
	}
}

// TestTwilioCLIRealtimeMarkEchoRoundTrip drives the entrypoint as a
// subprocess, from a working directory that is NOT the repo root, with two
// relative path arguments (-audio and -record), and asserts the mark a
// realtime consumer requested comes back echoed after the CLI's playout
// estimate.
//
// The timing is the assertion, not merely the arrival. The engine bounds its
// own wait at the queued playout plus the mark-echo grace, so a record whose
// TimedOut is set would mean the CLI never answered inside that window and the
// engine gave up — which is exactly what a run against a peer that does not
// honour the mark protocol looks like. Asserting only that some record arrived
// is green against that.
func TestTwilioCLIRealtimeMarkEchoRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and runs a full call; skipped under -short")
	}

	bin, _ := buildTwilioCLI(t, "twilio-cli-aatk105")

	be := newCLIRealtimeBackend(t)

	const authToken = "aatk105-test-token"
	marks := make(chan string, 4)
	echoes := make(chan twilio.MarkEcho, 4)
	// The engine's own record of what reached the carrier, used here purely as
	// the signal that the audio the mark must queue behind has actually been
	// written. Reading it is what keeps the mark request causally AFTER the
	// audio rather than merely later in wall-clock time.
	written := make(chan twilio.CarrierAudio, 512)

	srv := &twilio.Server{
		AuthToken:    authToken,
		StreamScheme: "ws",
		HandleStream: func(ctx context.Context, conn *websocket.Conn, start twilio.Frame) error {
			return twilio.HandleStreamRealtime(ctx, conn, start, be.url(),
				twilio.WithMarkRequestChan(marks),
				twilio.WithMarkEchoChan(echoes),
				twilio.WithCarrierAudioChan(written),
			)
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/voice", srv.ServeHTTP)
	mux.HandleFunc("/streams", srv.ServeStreams)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// The caller side: ten seconds of μ-law silence, long enough that the CLI
	// stays on the call for the whole round trip. Silence rather than speech
	// because a machine with ffplay installed will actually render it.
	dir := t.TempDir()
	caller := bytes.Repeat([]byte{telephony.MuLawSilence}, 10*telephony.SampleRateHz)
	if err := os.WriteFile(filepath.Join(dir, "caller.ulaw"), caller, 0o644); err != nil {
		t.Fatalf("stage caller audio: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin,
		"-webhook", ts.URL+"/voice",
		"-audio", "./caller.ulaw",
		"-record", "./inbound.ulaw",
		"+15551234567",
	)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TWILIO_AUTH_TOKEN="+authToken)
	var out lockedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start entrypoint: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	waitUntilTrue(t, 30*time.Second, be.ready, "the realtime handshake never completed", &out)

	// Half a second of engine audio, so the CLI has real playout to wait out
	// before it may echo.
	const frames = 25
	delta := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{telephony.MuLawSilence}, muLawFrame20ms))
	for i := 0; i < frames; i++ {
		be.emit(t, delta)
	}
	for i := 0; i < frames; i++ {
		select {
		case <-written:
		case <-time.After(30 * time.Second):
			t.Fatalf("only %d of %d media frames reached the carrier\n%s", i, frames, out.String())
		}
	}

	start := time.Now()
	marks <- "e2e"

	select {
	case rec := <-echoes:
		elapsed := time.Since(start)
		if rec.Name != "e2e" {
			t.Fatalf("the echo must carry the requested name: got %q\n%s", rec.Name, out.String())
		}
		if rec.TimedOut {
			t.Fatalf("the CLI did not echo the mark inside the engine's bound (%s elapsed); the round trip is not closed\n%s",
				elapsed, out.String())
		}
		// echoMark waits out the queued playout before answering, so an echo
		// that came back instantly would mean the CLI answered before the
		// audio it is acknowledging had played.
		if elapsed < telephony.MuLawDuration(frames*muLawFrame20ms)/2 {
			t.Fatalf("the mark was echoed after only %s, well inside the %s of audio queued ahead of it\n%s",
				elapsed, telephony.MuLawDuration(frames*muLawFrame20ms), out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("no mark echo ever reached the consumer\n%s", out.String())
	}

	if got := out.String(); !strings.Contains(got, "mark") {
		t.Errorf("the CLI's log never mentions a mark, so the echo cannot have come from echoMark\n%s", got)
	}
}

// lockedBuffer is a bytes.Buffer the test reads while the subprocess writes
// into it. exec wires Stdout/Stderr straight through to an io.Writer from its
// own goroutines, so an unguarded buffer is a data race the detector sees.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitUntilTrue polls cond until it holds, failing with the subprocess's own
// output attached — a run that never got as far as the handshake has already
// said why in its log, and losing that makes the failure unreadable.
func waitUntilTrue(t *testing.T, d time.Duration, cond func() bool, why string, out *lockedBuffer) {
	t.Helper()
	deadline := time.After(d)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s\n%s", why, out.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
