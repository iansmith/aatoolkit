package driver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony/twilio"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// smsFrom sends the inbound SMS these tests post, and smsBody is its text. The
// body repeats the number, so a span that recorded the message would hold it.
const (
	smsFrom = "+15551234567"
	smsBody = "Call me back at +15551234567."
)

// serverSpanWrapper returns a WithHandlerWrapper function that does what
// otelhttp.NewHandler does in production, with the SDK alone: for each request
// it starts a SERVER span named after the route, from tp, serves the request
// with that span in its context, and ends the span when the handler returns.
func serverSpanWrapper(tp trace.TracerProvider) func(string, http.Handler) http.Handler {
	tracer := tp.Tracer("webhook-test")
	return func(route string, next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, span := tracer.Start(r.Context(), route, trace.WithSpanKind(trace.SpanKindServer))
			defer span.End()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// postInboundSMS serves a twilio.Server whose HandleSMS is handle through
// StartTwilioServer with opts -- the exported entry point, so the options have
// to survive the pass-through to newTwilioMux as well as be applied there --
// and posts it one inbound SMS from from with body, signed the way the
// provider signs it. It fails the test unless the answer is 200. HandleSMS
// runs inside ServeSMS, so when this returns, the handler has returned.
func postInboundSMS(t *testing.T, handle func(context.Context, twilio.InboundSMS), from, body string, opts ...TwilioServerOption) {
	t.Helper()
	s := &twilio.Server{
		AuthToken:    "authtoken",
		StreamScheme: "ws", // so the signature is reconstructed over http, matching httptest
		HandleSMS:    handle,
	}
	hs := StartTwilioServer("127.0.0.1:0", s, opts...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
	})
	ts := httptest.NewServer(hs.Handler)
	t.Cleanup(ts.Close)

	form := url.Values{
		"MessageSid": {"SM123"},
		"From":       {from},
		"Body":       {body},
	}
	rawURL := ts.URL + "/sms/inbound"
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Twilio-Signature", twilio.ComputeSignature("authtoken", rawURL, form))

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /sms/inbound: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /sms/inbound status = %d, want 200", resp.StatusCode)
	}
}

// webhookSpan fails the test unless rec holds exactly one ended SERVER span,
// the wrapper's span for /sms/inbound, and returns it.
func webhookSpan(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	var servers []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			servers = append(servers, s)
		}
	}
	if len(servers) != 1 {
		t.Fatalf("got %d server spans, want exactly 1: the wrapper's, for the one inbound SMS", len(servers))
	}
	if name := servers[0].Name(); name != "/sms/inbound" {
		t.Errorf("server span name = %q, want %q", name, "/sms/inbound")
	}
	return servers[0]
}

// requireChildOf fails unless child's parent is parent's span, in the same
// trace.
func requireChildOf(t *testing.T, child, parent sdktrace.ReadOnlySpan) {
	t.Helper()
	if child.Parent().SpanID() != parent.SpanContext().SpanID() ||
		child.SpanContext().TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("span %q has parent %v in trace %v; want %q's span %v in trace %v",
			child.Name(), child.Parent().SpanID(), child.SpanContext().TraceID(),
			parent.Name(), parent.SpanContext().SpanID(), parent.SpanContext().TraceID())
	}
}

// TestWithHandlerWrapper_SpanParentsSMSHandlerContext (DoD g): a wrapper that
// starts a server span the way otelhttp does puts that span in the context
// HandleSMS receives, so whatever the consumer starts from that context is
// its child.
func TestWithHandlerWrapper_SpanParentsSMSHandlerContext(t *testing.T) {
	rec, tp := newRecorder(t)
	got := make(chan trace.SpanContext, 1)
	postInboundSMS(t, func(ctx context.Context, _ twilio.InboundSMS) {
		got <- trace.SpanContextFromContext(ctx)
	}, smsFrom, smsBody, WithHandlerWrapper(serverSpanWrapper(tp)))

	var inHandler trace.SpanContext
	select {
	case inHandler = <-got:
	default:
		t.Fatal("HandleSMS was not called")
	}
	server := webhookSpan(t, rec)
	if !inHandler.IsValid() {
		t.Fatalf("HandleSMS ctx carries no span; want the wrapper's server span %v", server.SpanContext().SpanID())
	}
	if !inHandler.Equal(server.SpanContext()) {
		t.Errorf("HandleSMS ctx span = %v in trace %v; want the wrapper's server span %v in trace %v",
			inHandler.SpanID(), inHandler.TraceID(), server.SpanContext().SpanID(), server.SpanContext().TraceID())
	}
}

// inboundSMSTurn posts one inbound SMS through a span-starting wrapper to a
// HandleSMS that does what a consumer's does: it builds the turn's messages
// from the SMS and hands the turn to a goroutine, which streams the reply from
// a scripted upstream through h.WithContext(detach(ctx)). The goroutine starts
// the call only once the webhook has been answered, as a turn that outlives
// its handler does. It returns the webhook span, the recorder holding the
// model span, and the call's error.
func inboundSMSTurn(t *testing.T, detach func(context.Context) context.Context) (sdktrace.ReadOnlySpan, *tracetest.SpanRecorder, error) {
	t.Helper()
	upstream, _ := newScriptedSSEServer(okSteps()...)
	t.Cleanup(upstream.Close)
	h, rec := tracedHost(t, chatURL(upstream), Tier{Model: "test", MaxTokens: 64})

	answered := make(chan struct{})
	done := make(chan error, 1)
	postInboundSMS(t, func(ctx context.Context, m twilio.InboundSMS) {
		window, err := json.Marshal([]map[string]string{
			{"role": "system", "content": "The caller is " + m.From + "."},
			{"role": "user", "content": m.Body},
		})
		if err != nil {
			done <- err
			return
		}
		v := h.WithContext(detach(ctx))
		go func() {
			<-answered
			_, _, err := v.SendStream(window, "fast", func(string) {})
			done <- err
		}()
	}, smsFrom, smsBody, WithHandlerWrapper(serverSpanWrapper(h.tracerProvider)))
	close(answered)

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn's model call did not finish")
	}
	return webhookSpan(t, rec), rec, err
}

// TestInboundSMS_ModelSpanNestsUnderWebhookSpan: the ticket's goal end to end.
// A model call a consumer starts for an inbound SMS, under
// context.WithoutCancel of the handler's ctx, succeeds after the webhook has
// been answered, and its span is a child of the webhook's server span. With
// content capture off (the default), neither span holds the sender's number,
// though the messages sent upstream carry it twice.
func TestInboundSMS_ModelSpanNestsUnderWebhookSpan(t *testing.T) {
	withoutCaptureEnv(t)
	webhook, rec, err := inboundSMSTurn(t, context.WithoutCancel)
	if err != nil {
		t.Fatalf("model call under context.WithoutCancel(ctx): %v", err)
	}
	model := onlyModelSpan(t, rec)
	requireChildOf(t, model, webhook)

	// Test-only and unanchored, so it finds a number anywhere in a value.
	e164 := regexp.MustCompile(`\+[1-9]\d{6,14}`)
	if hits := scanSpans([]sdktrace.ReadOnlySpan{webhook, model}, nil, e164); len(hits) > 0 {
		t.Errorf("spans hold the sender's number:\n%s", strings.Join(hits, "\n"))
	}
}

// TestInboundSMS_DetachedCallNeedsWithoutCancel: the same turn under the raw
// handler ctx fails, because net/http cancels a request's context once its
// handler has returned. The call returns context.Canceled, and its span --
// still the webhook span's child, since a cancelled context keeps its values
// -- records error.type canceled. This is the caveat WithHandlerWrapper
// documents, and why the turn needs context.WithoutCancel.
func TestInboundSMS_DetachedCallNeedsWithoutCancel(t *testing.T) {
	webhook, rec, err := inboundSMSTurn(t, func(ctx context.Context) context.Context { return ctx })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("model call under the handler's own ctx after it returned: err = %v; want context.Canceled", err)
	}
	model := onlyModelSpan(t, rec)
	requireChildOf(t, model, webhook)
	requireAttrs(t, model, map[string]any{"error.type": "canceled"})
	if st := model.Status(); st.Code != codes.Error || st.Description != "" {
		t.Errorf("model span status = %v %q; want Error with an empty description", st.Code, st.Description)
	}
}
