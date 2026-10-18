package driver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/host"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The context-taking forms are reachable from both the Host and the view
// WithContext returns, and that view is still a whole host.Host.
var (
	_ host.ContextSender  = (*Host)(nil)
	_ host.ContextBuilder = (*Host)(nil)
	_ host.Host           = (*boundHost)(nil)
	_ host.ContextSender  = (*boundHost)(nil)
	_ host.ContextBuilder = (*boundHost)(nil)
)

// ctxKey keys the value a test puts on the caller's context to see whether it
// reaches the upstream request.
type ctxKey struct{}

// withCtxValue is a context carrying v under ctxKey.
func withCtxValue(v string) context.Context {
	return context.WithValue(context.Background(), ctxKey{}, v)
}

// ctxRecordingTransport answers every request itself with a one-delta stream,
// textLine("ok"), so no socket is opened, and records the request's context:
// its deadline, the value under ctxKey, and the span it carries.
type ctxRecordingTransport struct {
	mu          sync.Mutex
	calls       int
	deadline    time.Time
	hasDeadline bool
	value       any
	span        trace.SpanContext
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
	rt.span = trace.SpanContextFromContext(r.Context())
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(textLine("ok") + "\n")),
		Request:    r,
	}, nil
}

// last returns what the most recent request carried.
func (rt *ctxRecordingTransport) last() (calls int, deadline time.Time, hasDeadline bool, value any) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.calls, rt.deadline, rt.hasDeadline, rt.value
}

// lastSpan returns the span context the most recent request's context
// carried, invalid when it carried none.
func (rt *ctxRecordingTransport) lastSpan() trace.SpanContext {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.span
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

// TestSendAndSendStream_RunOnTheContextPath: the context-free methods run on
// SendStreamContext's path: their request goes out under the context that
// carries the call's model span, which only that path starts. They keep
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
			rec, tp := newRecorder(t)
			h.tracerProvider = tp // what New copies from Config.TracerProvider
			before := time.Now()
			if err := call(h); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			requireSendTimeoutDeadline(t, rt, before, time.Now())
			if _, _, _, v := rt.last(); v != nil {
				t.Errorf("request carried caller value %v; want none", v)
			}
			span := onlyModelSpan(t, rec)
			if got := rt.lastSpan(); !got.Equal(span.SpanContext()) {
				t.Errorf("request went out under span %v (valid %v); want the call's model span %v",
					got.SpanID(), got.IsValid(), span.SpanContext().SpanID())
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

// TestSendStreamContext_DoneContextStopsBufferedStream: once the caller's
// context is done, no onSegment call starts and the call fails with the
// context's error, even when the rest of the stream is already buffered and
// no network read is left to fail. That holds for lines still to be
// processed (a malformed one included, which would otherwise fail the call
// its own way), for segments still to be cut from the line in hand, and for
// the end-of-stream flush, and the span records the failure. sseServer
// writes each of these short bodies in one write and returns without
// flushing, so net/http sends it whole, with a Content-Length.
func TestSendStreamContext_DoneContextStopsBufferedStream(t *testing.T) {
	const s1, s2, s3 = "The quick brown fox jumps over the lazy dog.",
		" A second sentence follows the first one here.",
		" And a third sentence closes the whole reply."
	cases := []struct {
		name  string
		lines []string
		// ctx returns the call's context, a func the first onSegment call
		// runs, and the context's cancel.
		ctx      func() (context.Context, func(), context.CancelFunc)
		wantIs   error
		wantType string
	}{
		{
			name:     "cancelled with lines still buffered",
			lines:    []string{textLine(s1), textLine(s2), textLine(s3), finishLine("stop"), doneLine},
			ctx:      cancelOnFirstSegment,
			wantIs:   context.Canceled,
			wantType: "canceled",
		},
		{
			name:     "cancelled with segments left in the line",
			lines:    []string{textLine(s1 + s2 + s3), finishLine("stop"), doneLine},
			ctx:      cancelOnFirstSegment,
			wantIs:   context.Canceled,
			wantType: "canceled",
		},
		{
			// The line after the cancel would fail the call its own way.
			name:     "cancelled before a malformed line",
			lines:    []string{textLine(s1), `data: {not json`},
			ctx:      cancelOnFirstSegment,
			wantIs:   context.Canceled,
			wantType: "canceled",
		},
		{
			name:     "cancelled before the end-of-stream flush",
			lines:    []string{textLine(s1 + " and trailing words with no stop")},
			ctx:      cancelOnFirstSegment,
			wantIs:   context.Canceled,
			wantType: "canceled",
		},
		{
			name:  "deadline passed during onSegment",
			lines: []string{textLine(s1), textLine(s2), textLine(s3), finishLine("stop"), doneLine},
			ctx: func() (context.Context, func(), context.CancelFunc) {
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				return ctx, func() { <-ctx.Done() }, cancel
			},
			wantIs:   context.DeadlineExceeded,
			wantType: "timeout",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withoutCaptureEnv(t)
			srv := sseServer(tc.lines...)
			defer srv.Close()
			h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
			ctx, onFirst, cancel := tc.ctx()
			defer cancel()

			segments, late := 0, 0
			content, _, err := h.SendStreamContext(ctx, traceWindow, "fast", func(string) {
				if ctx.Err() != nil {
					late++
				}
				segments++
				if segments == 1 {
					onFirst()
				}
			})
			if late != 0 {
				t.Errorf("%d onSegment calls started after the context was done; want none", late)
			}
			if err == nil {
				t.Fatalf("no error after the context was done; content %q", content)
			}
			if !errors.Is(err, tc.wantIs) || !strings.HasPrefix(err.Error(), "reading SSE stream: ") {
				t.Errorf("error = %q; want it to start %q and wrap %v", err, "reading SSE stream: ", tc.wantIs)
			}
			span := onlyModelSpan(t, rec)
			requireAttrs(t, span, map[string]any{"error.type": tc.wantType})
			if st := span.Status(); st.Code != codes.Error || st.Description != "" {
				t.Errorf("status = %v %q, want Error with an empty description", st.Code, st.Description)
			}
		})
	}
}

// cancelOnFirstSegment is a case context for
// TestSendStreamContext_DoneContextStopsBufferedStream that the first
// onSegment call cancels.
func cancelOnFirstSegment() (context.Context, func(), context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return ctx, cancel, cancel
}

// TestSendStreamContext_DoneContextFailsWithItsCause: a context cancelled or
// timed out with a cause fails the call with an error that matches both that
// cause and the context's own error (context.Canceled or DeadlineExceeded),
// whichever site sees it first: the check before an already-buffered line,
// the check at the end of the stream, a network read blocked on a stalled
// upstream, or the request still waiting for its response headers. So
// errors.Is against the cause tells the caller's own stop apart from an
// upstream failure in every case, and errors.Is against context.Canceled or
// DeadlineExceeded still holds. A stream read that fails this way reads
// "reading SSE stream: " and the cause's own text at each of its sites, so
// the text does not depend on which one saw the context done. The span still
// records canceled or timeout.
// TestSendStreamContext_DoneContextErrorOverHTTP1AndHTTP2 runs the network
// sites over both protocols.
func TestSendStreamContext_DoneContextFailsWithItsCause(t *testing.T) {
	errStop := errors.New("caller stopped the turn")
	errSlow := errors.New("caller gave up waiting")
	const s1, s2 = "The quick brown fox jumps over the lazy dog.", " A second sentence follows the first one here."
	buffered := func() (string, func()) {
		srv := sseServer(textLine(s1), textLine(s2), finishLine("stop"), doneLine)
		return chatURL(srv), srv.Close
	}
	// lastLine ends the body with the line that cuts the first segment, so
	// the cancel lands after the last line and the end-of-stream check is
	// what sees it.
	lastLine := func() (string, func()) {
		srv := sseServer(textLine(s1))
		return chatURL(srv), srv.Close
	}
	stalled := func() (string, func()) {
		srv, _ := newScriptedSSEServer(sseStep{line: textLine(s1)}, sseStep{delay: 5 * time.Second, line: textLine(s2)})
		return chatURL(srv), srv.Close
	}
	noHeaders := func() (string, func()) {
		srv, _ := newScriptedSSEServer(sseStep{delay: 5 * time.Second, line: textLine(s1)})
		return chatURL(srv), srv.Close
	}
	cancelCause := func() (context.Context, func(), func()) {
		ctx, cancel := context.WithCancelCause(context.Background())
		return ctx, func() { cancel(errStop) }, func() { cancel(nil) }
	}
	timeoutCause := func() (context.Context, func(), func()) {
		ctx, cancel := context.WithTimeoutCause(context.Background(), 50*time.Millisecond, errSlow)
		return ctx, func() { <-ctx.Done() }, cancel
	}
	cases := []struct {
		name     string
		upstream func() (url string, closeFn func())
		// ctx returns the call's context, what ends it, and its cleanup.
		ctx func() (ctx context.Context, end func(), cleanup func())
		// inSegment ends the context from the first onSegment call;
		// otherwise it is ended 50ms in, from another goroutine.
		inSegment bool
		wantCause error
		wantType  string
		// wantText is the start of the error's text, and all of it when
		// wholeText is set. A request error names the URL, so only its
		// start is fixed.
		wantText  string
		wholeText bool
	}{
		{"cancel cause, lines already buffered", buffered, cancelCause, true, errStop, "canceled", "reading SSE stream: caller stopped the turn", true},
		{"cancel cause, after the last line", lastLine, cancelCause, true, errStop, "canceled", "reading SSE stream: caller stopped the turn", true},
		{"cancel cause, upstream stalled", stalled, cancelCause, true, errStop, "canceled", "reading SSE stream: caller stopped the turn", true},
		{"cancel cause, before the response headers", noHeaders, cancelCause, false, errStop, "canceled", "calling ", false},
		{"timeout cause, lines already buffered", buffered, timeoutCause, true, errSlow, "timeout", "reading SSE stream: caller gave up waiting", true},
		{"timeout cause, upstream stalled", stalled, timeoutCause, false, errSlow, "timeout", "reading SSE stream: caller gave up waiting", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withoutCaptureEnv(t)
			url, closeFn := tc.upstream()
			defer closeFn()
			h, rec := tracedHost(t, url, Tier{Model: "test", MaxTokens: 512})
			ctx, end, cleanup := tc.ctx()
			defer cleanup()
			if !tc.inSegment {
				timer := time.AfterFunc(50*time.Millisecond, end)
				defer timer.Stop()
			}
			segments := 0
			_, _, err := h.SendStreamContext(ctx, traceWindow, "fast", func(string) {
				segments++
				if tc.inSegment && segments == 1 {
					end()
				}
			})
			if err == nil {
				t.Fatal("no error after the context was done")
			}
			if !errors.Is(err, tc.wantCause) {
				t.Errorf("error %q does not wrap the context's cause %q", err, tc.wantCause)
			}
			if ctxErr := ctx.Err(); !errors.Is(err, ctxErr) {
				t.Errorf("error %q does not wrap the context's error %v", err, ctxErr)
			}
			if tc.wholeText && err.Error() != tc.wantText {
				t.Errorf("error = %q, want %q", err, tc.wantText)
			} else if !strings.HasPrefix(err.Error(), tc.wantText) {
				t.Errorf("error = %q; want it to start %q", err, tc.wantText)
			}
			requireAttrs(t, onlyModelSpan(t, rec), map[string]any{"error.type": tc.wantType})
		})
	}
}

// TestSendStreamContext_DoneContextErrorOverHTTP1AndHTTP2: when the context
// is done while the call waits on the network -- for the response headers,
// in a read blocked on a stalled stream, or reading a non-200 status body --
// the error matches both the context's cause and its own error under
// errors.Is, over HTTP/1 and HTTP/2 alike. net/http's HTTP/1 transport fails
// such a wait with the cause and its HTTP/2 transport with ctx.Err(), and a
// status body cut short fails with neither, so the answer would otherwise
// depend on the protocol and the site. A stream read fails with the cause's
// own text over both protocols, and a status error keeps its text. The span
// records canceled or timeout. The context ends 50ms in, from another
// goroutine, while the wait is blocked.
func TestSendStreamContext_DoneContextErrorOverHTTP1AndHTTP2(t *testing.T) {
	errStop := errors.New("caller stopped the turn")
	errSlow := errors.New("caller gave up waiting")
	const s1, s2 = "The quick brown fox jumps over the lazy dog.", " A second sentence follows the first one here."
	protocols := []struct {
		name  string
		major int
		start func(http.Handler) *httptest.Server
	}{
		{"HTTP/1", 1, httptest.NewServer},
		{"HTTP/2", 2, func(h http.Handler) *httptest.Server {
			srv := httptest.NewUnstartedServer(h)
			srv.EnableHTTP2 = true
			srv.StartTLS()
			return srv
		}},
	}
	noHeaders := func() http.Handler {
		return scriptedSSEHandler(&sseRequestLog{}, sseStep{delay: 5 * time.Second, line: textLine(s1)})
	}
	stalled := func() http.Handler {
		return scriptedSSEHandler(&sseRequestLog{}, sseStep{line: textLine(s1)}, sseStep{delay: 5 * time.Second, line: textLine(s2)})
	}
	statusBody := func() http.Handler { return stalledStatusHandler(http.StatusTooManyRequests) }
	// Each returns the call's context and its cleanup; the context is done
	// 50ms in.
	cancelCause := func() (context.Context, func()) {
		ctx, cancel := context.WithCancelCause(context.Background())
		timer := time.AfterFunc(50*time.Millisecond, func() { cancel(errStop) })
		return ctx, func() { timer.Stop(); cancel(nil) }
	}
	timeoutCause := func() (context.Context, func()) {
		ctx, cancel := context.WithTimeoutCause(context.Background(), 50*time.Millisecond, errSlow)
		return ctx, cancel
	}
	readText := func(cause error) func(string) string {
		return func(string) string { return "reading SSE stream: " + cause.Error() }
	}
	statusText := func(url string) string { return "llm " + url + ": status 429 ()" }
	cases := []struct {
		name      string
		upstream  func() http.Handler
		ctx       func() (context.Context, func())
		wantCause error
		wantType  string
		// wantText is the error's text: exactly, or as a prefix when the
		// rest comes from the network stack.
		wantText   func(url string) string
		prefixOnly bool
	}{
		{"cancel cause, waiting for the response headers", noHeaders, cancelCause, errStop, "canceled",
			func(url string) string { return "calling " + url + ": " }, true},
		{"timeout cause, waiting for the response headers", noHeaders, timeoutCause, errSlow, "timeout",
			func(url string) string { return "calling " + url + ": " }, true},
		{"cancel cause, read blocked on a stalled stream", stalled, cancelCause, errStop, "canceled", readText(errStop), false},
		{"timeout cause, read blocked on a stalled stream", stalled, timeoutCause, errSlow, "timeout", readText(errSlow), false},
		{"cancel cause, reading a status body", statusBody, cancelCause, errStop, "canceled", statusText, false},
		{"timeout cause, reading a status body", statusBody, timeoutCause, errSlow, "timeout", statusText, false},
	}
	for _, p := range protocols {
		for _, tc := range cases {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				withoutCaptureEnv(t)
				var major atomic.Int32
				upstream := tc.upstream()
				srv := p.start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					major.Store(int32(r.ProtoMajor))
					upstream.ServeHTTP(w, r)
				}))
				defer srv.Close()
				h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
				h.client.Transport = srv.Client().Transport
				ctx, cleanup := tc.ctx()
				defer cleanup()

				_, _, err := h.SendStreamContext(ctx, traceWindow, "fast", func(string) {})
				if got := major.Load(); got != int32(p.major) {
					t.Fatalf("the upstream saw HTTP/%d; want HTTP/%d", got, p.major)
				}
				if err == nil {
					t.Fatal("no error after the context was done")
				}
				if !errors.Is(err, tc.wantCause) {
					t.Errorf("error %q does not wrap the context's cause %q", err, tc.wantCause)
				}
				if ctxErr := ctx.Err(); !errors.Is(err, ctxErr) {
					t.Errorf("error %q does not wrap the context's error %v", err, ctxErr)
				}
				want := tc.wantText(chatURL(srv))
				if tc.prefixOnly {
					if !strings.HasPrefix(err.Error(), want) {
						t.Errorf("error = %q; want it to start %q", err, want)
					}
				} else if err.Error() != want {
					t.Errorf("error = %q; want exactly %q", err, want)
				}
				requireAttrs(t, onlyModelSpan(t, rec), map[string]any{"error.type": tc.wantType})
			})
		}
	}
}

// TestWithContext_SendAndSendStreamRunUnderBoundContext: the view WithContext
// returns runs Send and SendStream under the bound context -- its values and
// its deadline reach the request -- and its history is the receiver's. The
// view's Context also runs under the bound context; that is
// TestWithContext_ContextUsesBoundCtx's to check.
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
			ctx, cancel := context.WithTimeout(withCtxValue("bound"), time.Minute)
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

	t.Run("history is the receiver's", func(t *testing.T) {
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
	cs, ok := h.WithContext(withCtxValue("bound")).(host.ContextSender)
	if !ok {
		t.Fatal("the WithContext view is not a host.ContextSender")
	}
	explicit := withCtxValue("explicit")

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
