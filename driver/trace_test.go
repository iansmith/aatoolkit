package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/host"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// traceWindow is the messages array the span tests send.
var traceWindow = []byte(`[{"role":"user","content":"hello"}]`)

// roleOnlyLine is the first chunk a role-first upstream sends: a role and
// empty content, no text.
const roleOnlyLine = `data: {"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`

// doneLine ends every scripted stream.
const doneLine = `data: [DONE]`

// textLine is a chunk carrying one content delta and no finish reason.
func textLine(text string) string {
	b, _ := json.Marshal(text)
	return `data: {"choices":[{"delta":{"content":` + string(b) + `},"finish_reason":null}]}`
}

// finishLine is a chunk with an empty delta and the given finish reason.
func finishLine(reason string) string {
	return `data: {"choices":[{"delta":{},"finish_reason":"` + reason + `"}]}`
}

// okSteps is a minimal successful stream: one content chunk that ends in a
// segment boundary, a stop finish, and [DONE].
func okSteps() []sseStep {
	return []sseStep{
		{line: textLine("The quick brown fox jumps over the lazy dog.")},
		{line: finishLine("stop")},
		{line: doneLine},
	}
}

// chatURL is the chat-completions URL on a test server.
func chatURL(srv *httptest.Server) string { return srv.URL + "/v1/chat/completions" }

// serverPort is the port a test server listens on.
func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing %s: %v", srv.URL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %s: %v", srv.URL, err)
	}
	return p
}

// startAttrs is what every model span sent to srv's "fast" tier with Model
// "test" carries from its start.
func startAttrs(t *testing.T, srv *httptest.Server) map[string]any {
	return map[string]any{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "openai",
		"gen_ai.request.model":  "test",
		"gen_ai.request.stream": true,
		"server.address":        "127.0.0.1",
		"server.port":           serverPort(t, srv),
		"aatoolkit.llm.tier":    "fast",
	}
}

// requireAttrs fails for every key in want that span lacks or holds another
// value under. An int in want is compared as the int64 the SDK stores.
func requireAttrs(t *testing.T, span sdktrace.ReadOnlySpan, want map[string]any) {
	t.Helper()
	got := attrMap(span)
	for k, w := range want {
		v, ok := got[k]
		if !ok {
			t.Errorf("attribute %s missing; want %v", k, w)
			continue
		}
		if i, isInt := w.(int); isInt {
			w = int64(i)
		}
		if !reflect.DeepEqual(v.AsInterface(), w) {
			t.Errorf("attribute %s = %#v, want %#v", k, v.AsInterface(), w)
		}
	}
}

// requireNoAttrs fails for every key in keys that span carries.
func requireNoAttrs(t *testing.T, span sdktrace.ReadOnlySpan, keys ...string) {
	t.Helper()
	got := attrMap(span)
	for _, k := range keys {
		if v, ok := got[k]; ok {
			t.Errorf("attribute %s = %v; want it absent", k, v.Emit())
		}
	}
}

// TestSendStreamContext_OneClientSpanUnderCaller (DoD a): one model request
// sent under a caller's span produces exactly one CLIENT span, its child,
// carrying the request and response attributes, the two first-arrival times,
// and an unset status -- and the call's result is what it was without tracing.
func TestSendStreamContext_OneClientSpanUnderCaller(t *testing.T) {
	const gap = 20 * time.Millisecond
	srv, _ := newScriptedSSEServer(
		sseStep{line: roleOnlyLine},
		sseStep{delay: gap, line: `data: {"id":"r1","model":"served-model","choices":[{"delta":{"content":"The quick brown fox jumps over the lazy dog."},"finish_reason":null}]}`},
		sseStep{line: `data: {"id":"r1","model":"served-model","choices":[{"delta":{"content":" Done."},"finish_reason":null}]}`},
		sseStep{line: `data: {"id":"r1","model":"served-model","choices":[{"delta":{},"finish_reason":"stop"}]}`},
		sseStep{line: doneLine},
	)
	defer srv.Close()
	h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})

	ctx, caller := h.tracerProvider.Tracer("test").Start(context.Background(), "caller")
	var segs []string
	content, reasoning, err := h.SendStreamContext(ctx, traceWindow, "fast", func(s string) { segs = append(segs, s) })
	caller.End()
	if err != nil {
		t.Fatalf("SendStreamContext: %v", err)
	}
	if string(content) != "The quick brown fox jumps over the lazy dog. Done." || len(reasoning) != 0 {
		t.Errorf("content, reasoning = %q, %q; want the streamed text and no reasoning", content, reasoning)
	}
	if want := []string{"The quick brown fox jumps over the lazy dog.", " Done."}; !reflect.DeepEqual(segs, want) {
		t.Errorf("segments = %q, want %q", segs, want)
	}

	span := onlyModelSpan(t, rec)
	if span.Parent().SpanID() != caller.SpanContext().SpanID() || span.SpanContext().TraceID() != caller.SpanContext().TraceID() {
		t.Errorf("model span parent %v trace %v; want the caller's span %v in trace %v",
			span.Parent().SpanID(), span.SpanContext().TraceID(), caller.SpanContext().SpanID(), caller.SpanContext().TraceID())
	}
	if span.SpanKind() != trace.SpanKindClient {
		t.Errorf("kind = %v, want client", span.SpanKind())
	}
	if span.Name() != "chat test" {
		t.Errorf("name = %q, want %q", span.Name(), "chat test")
	}
	if sc := span.InstrumentationScope(); sc.Name != "github.com/iansmith/aatoolkit/driver" || sc.SchemaURL != "https://opentelemetry.io/schemas/1.41.0" {
		t.Errorf("scope = %q schema %q; want the driver package and semconv 1.41.0", sc.Name, sc.SchemaURL)
	}
	requireAttrs(t, span, startAttrs(t, srv))
	requireAttrs(t, span, map[string]any{
		"gen_ai.response.model":          "served-model",
		"gen_ai.response.id":             "r1",
		"gen_ai.response.finish_reasons": []string{"stop"},
		"gen_ai.request.max_tokens":      512,
	})
	requireNoAttrs(t, span, "error.type")
	if span.Status().Code != codes.Unset {
		t.Errorf("status = %v %q, want unset", span.Status().Code, span.Status().Description)
	}

	attrs := attrMap(span)
	firstChunk, firstContent := attrs["gen_ai.response.time_to_first_chunk"], attrs["aatoolkit.llm.time_to_first_content"]
	if firstChunk.AsFloat64() <= 0 {
		t.Errorf("gen_ai.response.time_to_first_chunk = %v, want > 0", firstChunk.Emit())
	}
	if d := firstContent.AsFloat64() - firstChunk.AsFloat64(); d < 0.015 {
		t.Errorf("time_to_first_content %v - time_to_first_chunk %v = %v s; want >= 0.015 (the scripted %v gap)",
			firstContent.Emit(), firstChunk.Emit(), d, gap)
	}
}

// TestSendStreamContext_TimeToFirstContentAfterRoleOnlyChunk: on a role-first
// upstream the first chunk arrives at once and the first text later; the
// span's time to first content is the later one, not the first chunk's.
func TestSendStreamContext_TimeToFirstContentAfterRoleOnlyChunk(t *testing.T) {
	srv, _ := newScriptedSSEServer(
		sseStep{line: roleOnlyLine},
		sseStep{delay: 60 * time.Millisecond, line: textLine("Hello there, this is the first content.")},
		sseStep{line: finishLine("stop")},
		sseStep{line: doneLine},
	)
	defer srv.Close()
	h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
	if _, _, err := h.SendStream(traceWindow, "fast", func(string) {}); err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	attrs := attrMap(onlyModelSpan(t, rec))
	firstChunk, okChunk := attrs["gen_ai.response.time_to_first_chunk"]
	firstContent, okContent := attrs["aatoolkit.llm.time_to_first_content"]
	if !okChunk || !okContent {
		t.Fatalf("first-chunk present %v, first-content present %v; want both", okChunk, okContent)
	}
	if !(firstChunk.AsFloat64() < 0.05 && 0.05 <= firstContent.AsFloat64()) {
		t.Errorf("time_to_first_chunk %v, time_to_first_content %v; want first chunk < 0.05 <= first content",
			firstChunk.Emit(), firstContent.Emit())
	}
}

// TestSendStreamContext_FirstContentExcludesOnSegment: the first-arrival
// times are taken before a line is processed, so a slow onSegment fired by
// the first content line does not count toward time to first content; it
// does count toward the span's duration, which covers the whole call.
func TestSendStreamContext_FirstContentExcludesOnSegment(t *testing.T) {
	const slow = 80 * time.Millisecond
	srv, _ := newScriptedSSEServer(okSteps()...)
	defer srv.Close()
	h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
	segments := 0
	if _, _, err := h.SendStream(traceWindow, "fast", func(string) { segments++; time.Sleep(slow) }); err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	if segments != 1 {
		t.Fatalf("got %d segments, want 1 from the first content line", segments)
	}
	span := onlyModelSpan(t, rec)
	firstContent, ok := attrMap(span)["aatoolkit.llm.time_to_first_content"]
	if !ok {
		t.Fatal("aatoolkit.llm.time_to_first_content missing")
	}
	if firstContent.AsFloat64() >= slow.Seconds() {
		t.Errorf("time_to_first_content = %v s; want under the %v onSegment took", firstContent.Emit(), slow)
	}
	if d := span.EndTime().Sub(span.StartTime()); d < slow {
		t.Errorf("span lasted %v; want at least the %v onSegment took", d, slow)
	}
}

// TestSendAndSendStream_ProduceOrphanRootSpan (DoD c): the context-free
// methods run on the traced path with no caller span, so each gives one model
// span with no parent and the same start attributes.
func TestSendAndSendStream_ProduceOrphanRootSpan(t *testing.T) {
	calls := map[string]func(h *Host) error{
		"Send": func(h *Host) error {
			_, _, err := h.Send(traceWindow, "fast")
			return err
		},
		"SendStream": func(h *Host) error {
			_, _, err := h.SendStream(traceWindow, "fast", func(string) {})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			srv, _ := newScriptedSSEServer(okSteps()...)
			defer srv.Close()
			h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
			if err := call(h); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			span := onlyModelSpan(t, rec)
			if span.Parent().IsValid() {
				t.Errorf("model span has parent %v; want a root span", span.Parent().SpanID())
			}
			requireAttrs(t, span, startAttrs(t, srv))
		})
	}
}

// TestWithContext_SpansParentToBoundContext: Send and SendStream on the view
// WithContext returns produce spans under the bound context's span.
func TestWithContext_SpansParentToBoundContext(t *testing.T) {
	calls := map[string]func(v host.Host) error{
		"Send": func(v host.Host) error {
			_, _, err := v.Send(traceWindow, "fast")
			return err
		},
		"SendStream": func(v host.Host) error {
			_, _, err := v.SendStream(traceWindow, "fast", func(string) {})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			srv, _ := newScriptedSSEServer(okSteps()...)
			defer srv.Close()
			h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
			ctx, caller := h.tracerProvider.Tracer("test").Start(context.Background(), "inbound request")
			err := call(h.WithContext(ctx))
			caller.End()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			span := onlyModelSpan(t, rec)
			if span.Parent().SpanID() != caller.SpanContext().SpanID() {
				t.Errorf("model span parent = %v (valid %v); want the bound context's span %v",
					span.Parent().SpanID(), span.Parent().IsValid(), caller.SpanContext().SpanID())
			}
		})
	}
}

// TestModelSpan_UsageBecomesTokenAttributes (DoD d): token usage the stream
// carries -- on a final choice-less chunk or on the finish chunk, asked for
// or not -- lands on the span as the input and output token counts, without
// disturbing the finish reason; a stream without usage records neither.
func TestModelSpan_UsageBecomesTokenAttributes(t *testing.T) {
	cases := []struct {
		name         string
		includeUsage bool
		steps        []sseStep
		wantUsage    bool
		wantIn       int
		wantOut      int
		wantFinish   []string
	}{
		{
			name:         "choice-less usage chunk after finish length",
			includeUsage: true,
			steps: []sseStep{
				{line: `data: {"choices":[{"delta":{"content":"Cut short"},"finish_reason":null}],"usage":null}`},
				{line: `data: {"choices":[{"delta":{},"finish_reason":"length"}],"usage":null}`},
				{line: `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}`},
				{line: doneLine},
			},
			wantUsage: true, wantIn: 10, wantOut: 8, wantFinish: []string{"length"},
		},
		{
			name:         "usage on the finish chunk",
			includeUsage: true,
			steps: []sseStep{
				{line: textLine("All of it.")},
				{line: `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`},
				{line: doneLine},
			},
			wantUsage: true, wantIn: 7, wantOut: 3, wantFinish: []string{"stop"},
		},
		{
			name:         "flag off but the upstream sends usage",
			includeUsage: false,
			steps: []sseStep{
				{line: `data: {"choices":[{"delta":{"content":"Unasked."},"finish_reason":"stop"}],"usage":null}`},
				{line: `data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5}}`},
				{line: doneLine},
			},
			wantUsage: true, wantIn: 12, wantOut: 5, wantFinish: []string{"stop"},
		},
		{
			name:         "no usage",
			includeUsage: true,
			steps:        okSteps(),
			wantUsage:    false, wantFinish: []string{"stop"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newScriptedSSEServer(tc.steps...)
			defer srv.Close()
			h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512, IncludeUsage: tc.includeUsage})
			if _, _, err := h.SendStream(traceWindow, "fast", func(string) {}); err != nil {
				t.Fatalf("SendStream: %v", err)
			}
			span := onlyModelSpan(t, rec)
			requireAttrs(t, span, map[string]any{"gen_ai.response.finish_reasons": tc.wantFinish})
			if tc.wantUsage {
				requireAttrs(t, span, map[string]any{
					"gen_ai.usage.input_tokens":  tc.wantIn,
					"gen_ai.usage.output_tokens": tc.wantOut,
				})
			} else {
				requireNoAttrs(t, span, "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens")
			}
		})
	}
}

// TestCaptureMessageContent_OffByDefault (DoD e): unless the capture variable
// parses as true, a model span carries no message content.
func TestCaptureMessageContent_OffByDefault(t *testing.T) {
	settings := []struct {
		name  string
		set   bool
		value string
	}{
		{name: "unset"},
		{name: "empty", set: true, value: ""},
		{name: "zero", set: true, value: "0"},
		{name: "false", set: true, value: "false"},
		{name: "unparsable", set: true, value: "yes please"},
	}
	for _, s := range settings {
		t.Run(s.name, func(t *testing.T) {
			if s.set {
				withCaptureEnv(t, s.value)
			} else {
				withoutCaptureEnv(t)
			}
			srv, _ := newScriptedSSEServer(okSteps()...)
			defer srv.Close()
			h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
			if _, _, err := h.SendStream(traceWindow, "fast", func(string) {}); err != nil {
				t.Fatalf("SendStream: %v", err)
			}
			requireNoAttrs(t, onlyModelSpan(t, rec), "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions")
		})
	}
}

// TestCaptureMessageContent_OnRecordsGenAIMessages (DoD e): with capture on,
// the span carries the request's messages and the model's output as GenAI
// message JSON. System messages stay in the input; array content keeps its
// text parts; reasoning is its own part; partial output on an error is
// recorded, and none is invented when the stream failed before any.
func TestCaptureMessageContent_OnRecordsGenAIMessages(t *testing.T) {
	type result struct {
		input, output       string
		hasInput, hasOutput bool
		errType             string
	}
	run := func(t *testing.T, capture string, window []byte, steps ...sseStep) result {
		t.Helper()
		withCaptureEnv(t, capture)
		srv, _ := newScriptedSSEServer(steps...)
		defer srv.Close()
		h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
		_, _, _ = h.SendStream(window, "fast", func(string) {})
		attrs := attrMap(onlyModelSpan(t, rec))
		in, hasIn := attrs["gen_ai.input.messages"]
		out, hasOut := attrs["gen_ai.output.messages"]
		et := attrs["error.type"]
		return result{input: in.AsString(), output: out.AsString(), hasInput: hasIn, hasOutput: hasOut, errType: et.AsString()}
	}

	t.Run("a Host.Context window", func(t *testing.T) {
		h := New(Config{
			Prompt:      func() string { return "You are terse." },
			UserContext: func(string) string { return "The caller prefers mornings." },
		})
		h.Remember("user", []byte("What time is it?"))
		window := h.Context("What time is it?")
		r := run(t, "1", window,
			sseStep{line: textLine("It is noon.")},
			sseStep{line: finishLine("stop")},
			sseStep{line: doneLine},
		)
		wantIn := `[{"role":"system","parts":[{"type":"text","content":"You are terse."}]},` +
			`{"role":"system","parts":[{"type":"text","content":"The caller prefers mornings."}]},` +
			`{"role":"user","parts":[{"type":"text","content":"What time is it?"}]}]`
		if r.input != wantIn {
			t.Errorf("gen_ai.input.messages =\n%s\nwant\n%s", r.input, wantIn)
		}
		wantOut := `[{"role":"assistant","parts":[{"type":"text","content":"It is noon."}],"finish_reason":"stop"}]`
		if r.output != wantOut {
			t.Errorf("gen_ai.output.messages =\n%s\nwant\n%s", r.output, wantOut)
		}
	})

	t.Run("true turns it on", func(t *testing.T) {
		r := run(t, "true", traceWindow, okSteps()...)
		if !r.hasInput || !r.hasOutput {
			t.Errorf("input present %v, output present %v with the variable \"true\"; want both", r.hasInput, r.hasOutput)
		}
	})

	t.Run("array content keeps its text parts", func(t *testing.T) {
		window := []byte(`[{"role":"user","content":[{"type":"text","text":"Look at this."},{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]},` +
			`{"role":"assistant","content":null},{"role":"tool"}]`)
		r := run(t, "1", window, okSteps()...)
		want := `[{"role":"user","parts":[{"type":"text","content":"Look at this."}]},{"role":"assistant","parts":[]},{"role":"tool","parts":[]}]`
		if r.input != want {
			t.Errorf("gen_ai.input.messages =\n%s\nwant\n%s", r.input, want)
		}
	})

	t.Run("a window that is not an array of objects is omitted", func(t *testing.T) {
		r := run(t, "1", []byte(`["just a string"]`), okSteps()...)
		if r.hasInput {
			t.Errorf("gen_ai.input.messages = %s; want it omitted", r.input)
		}
		if !r.hasOutput {
			t.Error("gen_ai.output.messages missing; the output does not depend on the window")
		}
	})

	t.Run("reasoning is its own part", func(t *testing.T) {
		r := run(t, "1", traceWindow,
			sseStep{line: `data: {"choices":[{"delta":{"reasoning_content":"Thinking it over."},"finish_reason":null}]}`},
			sseStep{line: `data: {"choices":[{"delta":{"content":"Answer."},"finish_reason":"stop"}]}`},
			sseStep{line: doneLine},
		)
		want := `[{"role":"assistant","parts":[{"type":"reasoning","content":"Thinking it over."},{"type":"text","content":"Answer."}],"finish_reason":"stop"}]`
		if r.output != want {
			t.Errorf("gen_ai.output.messages =\n%s\nwant\n%s", r.output, want)
		}
	})

	t.Run("partial output on a mid-stream error", func(t *testing.T) {
		r := run(t, "1", traceWindow,
			sseStep{line: textLine("Partial answer")},
			sseStep{line: `data: {"error":{"message":"upstream overloaded"}}`},
		)
		if r.errType != "upstream_error" {
			t.Errorf("error.type = %q, want upstream_error", r.errType)
		}
		want := `[{"role":"assistant","parts":[{"type":"text","content":"Partial answer"}],"finish_reason":"error"}]`
		if r.output != want {
			t.Errorf("gen_ai.output.messages =\n%s\nwant\n%s", r.output, want)
		}
	})

	t.Run("no output when failing before content", func(t *testing.T) {
		r := run(t, "1", traceWindow,
			sseStep{line: `data: {"error":{"message":"upstream overloaded"}}`},
		)
		if r.errType != "upstream_error" {
			t.Errorf("error.type = %q, want upstream_error", r.errType)
		}
		if r.hasOutput {
			t.Errorf("gen_ai.output.messages = %s; want none when no output arrived", r.output)
		}
		if !r.hasInput {
			t.Error("gen_ai.input.messages missing; the request was sent")
		}
	})
}

// TestModelSpan_EndsOnceAfterLastSegment: the span ends after the call's last
// onSegment, the end-of-stream flush included, and a panicking onSegment
// still ends it -- as an error of type _OTHER -- while the panic propagates.
func TestModelSpan_EndsOnceAfterLastSegment(t *testing.T) {
	t.Run("after the last segment", func(t *testing.T) {
		srv, _ := newScriptedSSEServer(
			sseStep{line: textLine("The quick brown fox jumps over the lazy dog.")},
			sseStep{line: textLine(" A trailing clause with no stop")},
			sseStep{line: finishLine("stop")},
			sseStep{line: doneLine},
		)
		defer srv.Close()
		h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
		var stamps []time.Time
		if _, _, err := h.SendStream(traceWindow, "fast", func(string) { stamps = append(stamps, time.Now()) }); err != nil {
			t.Fatalf("SendStream: %v", err)
		}
		if len(stamps) != 2 {
			t.Fatalf("got %d segments, want 2 (one at the boundary, one from the end-of-stream flush)", len(stamps))
		}
		span := onlyModelSpan(t, rec)
		for i, at := range stamps {
			if at.After(span.EndTime()) {
				t.Errorf("segment %d at %v is after the span ended at %v", i, at, span.EndTime())
			}
		}
	})

	t.Run("a panicking onSegment", func(t *testing.T) {
		srv, _ := newScriptedSSEServer(okSteps()...)
		defer srv.Close()
		h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
		func() {
			defer func() {
				if recover() == nil {
					t.Error("onSegment's panic did not propagate out of SendStream")
				}
			}()
			_, _, _ = h.SendStream(traceWindow, "fast", func(string) { panic("onSegment failed") })
		}()
		span := onlyModelSpan(t, rec)
		requireAttrs(t, span, map[string]any{"error.type": "_OTHER"})
		if st := span.Status(); st.Code != codes.Error || st.Description != "" {
			t.Errorf("status = %v %q, want Error with an empty description", st.Code, st.Description)
		}
	})
}

// TestSendStreamContext_ConcurrentCallsOwnTheirSpans: concurrent calls on one
// Host each get their own span, under their own caller, carrying their own
// response. Run it under -race.
func TestSendStreamContext_ConcurrentCallsOwnTheirSpans(t *testing.T) {
	// The upstream answers each request with an id naming the caller, taken
	// from the request's one user message, so a span holding another call's
	// state shows up as the wrong id.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) != 1 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id := req.Messages[0].Content
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, line := range []string{
			`data: {"id":"` + id + `","choices":[{"delta":{"content":"Reply to ` + id + `,"},"finish_reason":null}]}`,
			`data: {"id":"` + id + `","choices":[{"delta":{"content":" done."},"finish_reason":"stop"}]}`,
			doneLine,
		} {
			_, _ = io.WriteString(w, line+"\n")
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer srv.Close()
	h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})

	const n = 8
	callers := make([]trace.SpanContext, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, caller := h.tracerProvider.Tracer("test").Start(context.Background(), fmt.Sprintf("caller-%d", i))
			callers[i] = caller.SpanContext()
			window := []byte(fmt.Sprintf(`[{"role":"user","content":"call-%d"}]`, i))
			_, _, errs[i] = h.SendStreamContext(ctx, window, "fast", func(string) {})
			caller.End()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}

	spans := modelSpans(rec)
	if len(spans) != n {
		t.Fatalf("got %d model spans, want %d", len(spans), n)
	}
	byParent := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		if _, dup := byParent[s.Parent().SpanID()]; dup {
			t.Errorf("two model spans share parent %v", s.Parent().SpanID())
		}
		byParent[s.Parent().SpanID()] = s
	}
	for i, c := range callers {
		s, ok := byParent[c.SpanID()]
		if !ok {
			t.Errorf("caller %d has no model span", i)
			continue
		}
		if s.SpanContext().TraceID() != c.TraceID() {
			t.Errorf("caller %d's model span is in trace %v, want %v", i, s.SpanContext().TraceID(), c.TraceID())
		}
		requireAttrs(t, s, map[string]any{"gen_ai.response.id": fmt.Sprintf("call-%d", i)})
	}
}

// TestSendStream_NoProviderIsNoOp: with no tracer provider configured and
// none installed globally, a call returns exactly what it did before tracing
// existed, through New and through a struct-literal Host alike.
func TestSendStream_NoProviderIsNoOp(t *testing.T) {
	srv, _ := newScriptedSSEServer(
		sseStep{line: roleOnlyLine},
		sseStep{line: textLine("The quick brown fox jumps over the lazy dog.")},
		sseStep{line: textLine(" Done.")},
		sseStep{line: finishLine("stop")},
		sseStep{line: doneLine},
	)
	defer srv.Close()
	tiers := map[string]Tier{"fast": {URL: chatURL(srv), Model: "test", MaxTokens: 512}}
	hosts := map[string]*Host{
		"New with a zero TracerProvider": New(Config{Tiers: tiers, Prompt: func() string { return "" }}),
		"struct literal":                 {client: srv.Client(), tiers: tiers},
	}
	for name, h := range hosts {
		t.Run(name, func(t *testing.T) {
			var segs []string
			content, reasoning, err := h.SendStream(traceWindow, "fast", func(s string) { segs = append(segs, s) })
			if err != nil {
				t.Fatalf("SendStream: %v", err)
			}
			if string(content) != "The quick brown fox jumps over the lazy dog. Done." || len(reasoning) != 0 {
				t.Errorf("content, reasoning = %q, %q", content, reasoning)
			}
			if want := []string{"The quick brown fox jumps over the lazy dog.", " Done."}; !reflect.DeepEqual(segs, want) {
				t.Errorf("segments = %q, want %q", segs, want)
			}
		})
	}
}

// TestModelSpan_NotRecordingSkipsCapture: on a span that is not recording,
// ending it does no work that grows with the request -- capture on or not,
// a 1000-message window costs no more allocations than a 1-message one. The
// control first shows that on a recording span with capture on it does, so
// the measurement can see capture's cost.
func TestModelSpan_NotRecordingSkipsCapture(t *testing.T) {
	withCaptureEnv(t, "1")
	msgs := make([]message, 1000)
	for i := range msgs {
		msgs[i] = message{Role: "user", Content: fmt.Sprintf("message number %d of the window", i)}
	}
	big, _ := json.Marshal(msgs)
	small, _ := json.Marshal(msgs[:1])
	st := &streamState{acc: newSegmentAccumulator(minSegmentChars), hasContent: true, finishReason: "stop"}
	st.contentBuf.WriteString("An answer.")

	endAllocs := func(tp trace.TracerProvider, window []byte) float64 {
		h := &Host{tracerProvider: tp}
		return testing.AllocsPerRun(20, func() {
			_, call := h.startModelSpan(context.Background(), "fast", "test", "http://127.0.0.1:8080/v1/chat/completions")
			call.end(st, 512, window, "")
		})
	}

	_, recording := newRecorder(t)
	if s, b := endAllocs(recording, small), endAllocs(recording, big); b <= s {
		t.Fatalf("control: on a recording span a 1000-message window took %v allocs and a 1-message one %v; "+
			"want more, or this test cannot see capture", b, s)
	}
	none := noop.NewTracerProvider()
	if s, b := endAllocs(none, small), endAllocs(none, big); b > s {
		t.Errorf("on a non-recording span a 1000-message window took %v allocs and a 1-message one %v; want no more", b, s)
	}
}
