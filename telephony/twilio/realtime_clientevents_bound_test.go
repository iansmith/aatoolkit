package twilio

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/realtime"
)

// Send-bound regression test for AATK-82.
//
// realtimeClientEventSendTimeout exists because the client-event write runs on
// HandleStreamRealtime's own select loop: a write that never completes there
// parks the loop, and backendDone, carrierDone and the idle timer all stop
// being observed along with it. The rest of this ticket's suite drives sends
// that SUCCEED or that FAIL; neither exercises the one that simply never
// returns, which is the case the bound was added for.

// stalledConn wraps a net.Conn so a test can park its Write calls on demand,
// modelling a backend that has stopped reading its socket — buffers fill and
// the write blocks, without the connection erroring or going away.
//
// A parked Write is released by Close, exactly as a real socket write is: the
// transport's own write timeout tears the connection down when the caller's
// context expires, and a fake that ignored Close would deadlock the teardown
// it is meant to be measuring rather than modelling it.
//
// It differs from faultyConn deliberately. That one makes a write FAIL, which
// returns immediately; this one makes a write HANG, which is the only way to
// exercise a bound on how long the engine will wait.
type stalledConn struct {
	net.Conn
	stall  *atomic.Bool
	closed chan struct{}
	once   sync.Once
}

func (s *stalledConn) Write(p []byte) (int, error) {
	if s.stall.Load() {
		<-s.closed
		return 0, net.ErrClosed
	}
	return s.Conn.Write(p)
}

func (s *stalledConn) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.Conn.Close()
}

// stalledHTTPClient returns an *http.Client whose dialed connections can have
// their Write calls parked on demand via the returned *atomic.Bool, and
// otherwise behave exactly like a normal connection.
func stalledHTTPClient() (*http.Client, *atomic.Bool) {
	var stall atomic.Bool
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return &stalledConn{Conn: c, stall: &stall, closed: make(chan struct{})}, nil
			},
		},
	}, &stall
}

// TestClientEventChan_SendThatNeverCompletesEndsCallOnItsBound pins what
// realtimeClientEventSendTimeout is for: a consumer-event write to a backend
// that has stopped reading must end the call on that bound rather than wedge
// HandleStreamRealtime's select loop.
//
// Unbounded, the write parks on the call's own context — which nothing
// cancels while the call is notionally still running — and the loop that
// observes every other way the call can end parks with it. waitDone failing on
// timeout is that hang; a non-nil error inside the window is the bound
// working.
//
// It also pins the ATTRIBUTION of that ending, which nothing else in this
// ticket's suite does. The transport's write error reads identically whether
// it came from forwarding carrier audio or a consumer event, so the send
// error is wrapped with its own prefix on the way out; delete the wrap and
// every other test in the package stays green (mutation-verified). Asserting
// only "some error" would also let this test pass for an ending that had
// nothing to do with the bound.
//
// slopstop:test regression — guards: "a consumer-event write that never completes is bounded, so it cannot park HandleStreamRealtime's select loop and silence backendDone, carrierDone and the idle timer, and the call ends with an error naming the client-event send rather than a bare transport error"
func TestClientEventChan_SendThatNeverCompletesEndsCallOnItsBound(t *testing.T) {
	hc, stall := stalledHTTPClient()
	origDial := dialRealtime
	dialRealtime = func(ctx context.Context, url string, opts ...realtime.DialOption) (*realtime.Client, error) {
		return origDial(ctx, url, append(opts, realtime.WithHTTPClient(hc))...)
	}
	defer func() { dialRealtime = origDial }()

	ch := make(chan json.RawMessage, 1)
	be := newFakeRealtimeBackend(t)
	h := newRealtimeHarnessWith(t, NewStreamHandler(be.url(), WithClientEventChan(ch)))
	waitBackendReady(t, be, h)

	stall.Store(true)
	ch <- clientEventRaw(60)

	// Generous slack over the bound itself: the assertion is that the call
	// ends at all, not that it ends to the millisecond.
	err := h.waitDone(realtimeClientEventSendTimeout + 5*time.Second)
	if err == nil {
		t.Fatal("a consumer-event write that never completes must end the call with a non-nil error on its own bound")
	}
	// The ending is deterministic, not a race with backendDone: the send
	// error is returned from inside the select case, before the loop can
	// reach any other case.
	if !strings.Contains(err.Error(), "twilio: realtime: client event send") {
		t.Fatalf("the call must end naming the client-event send in its error, got: %v", err)
	}
}
