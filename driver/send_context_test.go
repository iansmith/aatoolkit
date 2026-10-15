package driver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/host"
)

// The context-taking send forms are reachable from both the Host and the view
// WithContext returns, and that view is still a whole host.Host.
var (
	_ host.ContextSender = (*Host)(nil)
	_ host.Host          = (*boundHost)(nil)
	_ host.ContextSender = (*boundHost)(nil)
)

// ctxKey keys the value a test puts on the caller's context to see whether it
// reaches the upstream request.
type ctxKey struct{}

// okSSEStream is a one-delta stream that SendStream accepts.
const okSSEStream = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n"

// ctxRecordingTransport answers every request itself with okSSEStream, so no
// socket is opened, and records the request's context: its deadline and the
// value under ctxKey.
type ctxRecordingTransport struct {
	mu          sync.Mutex
	calls       int
	deadline    time.Time
	hasDeadline bool
	value       any
}

func (rt *ctxRecordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	dl, ok := r.Context().Deadline()
	rt.mu.Lock()
	rt.calls++
	rt.deadline, rt.hasDeadline = dl, ok
	rt.value = r.Context().Value(ctxKey{})
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(okSSEStream)),
		Request:    r,
	}, nil
}

// last returns what the most recent request carried.
func (rt *ctxRecordingTransport) last() (calls int, deadline time.Time, hasDeadline bool, value any) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.calls, rt.deadline, rt.hasDeadline, rt.value
}

// newCtxRecordingHost builds a Host through New and routes its client through
// a ctxRecordingTransport.
func newCtxRecordingHost() (*Host, *ctxRecordingTransport) {
	rt := &ctxRecordingTransport{}
	h := New(Config{
		Tiers: map[string]Tier{
			"fast": {URL: "http://upstream.invalid/v1/chat/completions", Model: "test", MaxTokens: 8},
		},
		Prompt: func() string { return "" },
	})
	h.client.Transport = rt
	return h, rt
}

// requireSendTimeoutDeadline fails unless the last request carried a deadline
// of sendTimeout after a moment between before and after.
func requireSendTimeoutDeadline(t *testing.T, rt *ctxRecordingTransport, before, after time.Time) {
	t.Helper()
	calls, dl, ok, _ := rt.last()
	if calls == 0 {
		t.Fatal("no request reached the transport")
	}
	if !ok {
		t.Fatal("request carried no deadline; want one sendTimeout out")
	}
	if dl.Before(before.Add(sendTimeout)) || dl.After(after.Add(sendTimeout)) {
		t.Errorf("deadline = %v, want within [%v, %v] (now + sendTimeout)",
			dl, before.Add(sendTimeout), after.Add(sendTimeout))
	}
}

// TestSendStreamContext_DeadlineComposition: the request's deadline is the
// earlier of the caller's and sendTimeout. A caller with no deadline, or a
// later one, is bounded at sendTimeout; an earlier caller deadline is kept
// exactly.
func TestSendStreamContext_DeadlineComposition(t *testing.T) {
	window := []byte(`[{"role":"user","content":"hi"}]`)

	t.Run("no caller deadline", func(t *testing.T) {
		h, rt := newCtxRecordingHost()
		before := time.Now()
		if _, _, err := h.SendStreamContext(context.Background(), window, "fast", func(string) {}); err != nil {
			t.Fatalf("SendStreamContext: %v", err)
		}
		requireSendTimeoutDeadline(t, rt, before, time.Now())
	})

	t.Run("earlier caller deadline", func(t *testing.T) {
		h, rt := newCtxRecordingHost()
		// Earlier than sendTimeout, and long enough that the in-memory call
		// never reaches it.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		want, _ := ctx.Deadline()
		if _, _, err := h.SendStreamContext(ctx, window, "fast", func(string) {}); err != nil {
			t.Fatalf("SendStreamContext: %v", err)
		}
		_, got, ok, _ := rt.last()
		if !ok || !got.Equal(want) {
			t.Errorf("deadline = %v (present %v), want the caller's %v", got, ok, want)
		}
	})

	t.Run("later caller deadline", func(t *testing.T) {
		h, rt := newCtxRecordingHost()
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		before := time.Now()
		if _, _, err := h.SendStreamContext(ctx, window, "fast", func(string) {}); err != nil {
			t.Fatalf("SendStreamContext: %v", err)
		}
		requireSendTimeoutDeadline(t, rt, before, time.Now())
	})
}

// TestSendAndSendStream_RunOnTheContextPath: the context-free methods keep
// today's bound -- a deadline sendTimeout out -- and carry no caller values.
func TestSendAndSendStream_RunOnTheContextPath(t *testing.T) {
	window := []byte(`[{"role":"user","content":"hi"}]`)
	calls := map[string]func(h *Host) error{
		"Send": func(h *Host) error {
			_, _, err := h.Send(window, "fast")
			return err
		},
		"SendStream": func(h *Host) error {
			_, _, err := h.SendStream(window, "fast", func(string) {})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			h, rt := newCtxRecordingHost()
			before := time.Now()
			if err := call(h); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			requireSendTimeoutDeadline(t, rt, before, time.Now())
			if _, _, _, v := rt.last(); v != nil {
				t.Errorf("request carried caller value %v; want none", v)
			}
		})
	}
}

// TestSendStreamContext_CallerCancelEndsCall: cancelling the caller's context
// mid-stream ends the call promptly with a cancellation error, instead of
// waiting out the upstream. The cancel happens inside onSegment, so it lands
// while the stream is open and its next line is still seconds away.
func TestSendStreamContext_CallerCancelEndsCall(t *testing.T) {
	const stall = 5 * time.Second
	srv, _ := newScriptedSSEServer(
		sseStep{line: `data: {"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`},
		sseStep{line: `data: {"choices":[{"delta":{"content":"The quick brown fox jumps over the lazy dog."},"finish_reason":null}]}`},
		sseStep{delay: stall, line: `data: {"choices":[{"delta":{"content":" More."},"finish_reason":"stop"}]}`},
		sseStep{line: `data: [DONE]`},
	)
	defer srv.Close()
	h := New(Config{
		Tiers:  map[string]Tier{"fast": {URL: srv.URL + "/v1/chat/completions", Model: "test", MaxTokens: 8}},
		Prompt: func() string { return "" },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	_, _, err := h.SendStreamContext(ctx, []byte(`[]`), "fast", func(string) { cancel() })
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("SendStreamContext returned no error after the caller cancelled (took %v)", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v; want it to wrap context.Canceled", err)
	}
	if elapsed >= stall/2 {
		t.Errorf("call took %v after cancel; want it to end well before the %v stall", elapsed, stall)
	}
}

// TestWithContext_SendAndSendStreamRunUnderBoundContext: the view WithContext
// returns runs Send and SendStream under the bound context -- its values and
// its deadline reach the request -- and shares every other method, so its
// history is the receiver's.
func TestWithContext_SendAndSendStreamRunUnderBoundContext(t *testing.T) {
	window := []byte(`[{"role":"user","content":"hi"}]`)
	calls := map[string]func(v host.Host) error{
		"Send": func(v host.Host) error {
			_, _, err := v.Send(window, "fast")
			return err
		},
		"SendStream": func(v host.Host) error {
			_, _, err := v.SendStream(window, "fast", func(string) {})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			h, rt := newCtxRecordingHost()
			ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), ctxKey{}, "bound"), time.Minute)
			defer cancel()
			want, _ := ctx.Deadline()
			if err := call(h.WithContext(ctx)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			_, dl, ok, v := rt.last()
			if v != "bound" {
				t.Errorf("request carried value %v; want the bound context's %q", v, "bound")
			}
			if !ok || !dl.Equal(want) {
				t.Errorf("deadline = %v (present %v); want the bound context's %v", dl, ok, want)
			}
		})
	}

	t.Run("other methods are the receiver's", func(t *testing.T) {
		h, _ := newCtxRecordingHost()
		v := h.WithContext(context.Background())
		v.Remember("assistant", []byte("from the view"))
		if got := string(h.LastAnswer()); got != "from the view" {
			t.Errorf("receiver's LastAnswer = %q; want the view's Remember to land on the receiver", got)
		}
	})

	t.Run("nil context panics", func(t *testing.T) {
		h, _ := newCtxRecordingHost()
		defer func() {
			if recover() == nil {
				t.Error("WithContext(nil) did not panic")
			}
		}()
		var nilCtx context.Context
		h.WithContext(nilCtx)
	})
}

// TestWithContext_ExplicitContextWins: on the bound view, SendContext and
// SendStreamContext use the context they are given, not the bound one.
func TestWithContext_ExplicitContextWins(t *testing.T) {
	window := []byte(`[{"role":"user","content":"hi"}]`)
	h, rt := newCtxRecordingHost()
	cs, ok := h.WithContext(context.WithValue(context.Background(), ctxKey{}, "bound")).(host.ContextSender)
	if !ok {
		t.Fatal("the WithContext view is not a host.ContextSender")
	}
	explicit := context.WithValue(context.Background(), ctxKey{}, "explicit")

	if _, _, err := cs.SendContext(explicit, window, "fast"); err != nil {
		t.Fatalf("SendContext: %v", err)
	}
	if _, _, _, v := rt.last(); v != "explicit" {
		t.Errorf("SendContext request carried %v; want the explicit context's %q", v, "explicit")
	}

	if _, _, err := cs.SendStreamContext(explicit, window, "fast", func(string) {}); err != nil {
		t.Fatalf("SendStreamContext: %v", err)
	}
	if _, _, _, v := rt.last(); v != "explicit" {
		t.Errorf("SendStreamContext request carried %v; want the explicit context's %q", v, "explicit")
	}
}
