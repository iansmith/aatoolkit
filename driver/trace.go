package driver

// ---------------------------------------------------------------------------
// driver: OpenTelemetry spans for model requests (AATK-159). This is the only
// non-test file that looks up a tracer or starts, annotates or ends a span;
// TestTraceSource_OnlyTraceGoTouchesSpans keeps the package's other files
// off spans. Its functions are free functions or methods on its own types,
// handed no type that is or holds a Tier (its APIKey), a Host (its tiers,
// history and prompt), a request, a header or an error (whose text can echo
// an upstream body): not as a parameter, a result, a field of a struct it is
// handed, or under an alias. TestTraceSource_NoSecretsReachSpanCode checks
// those types. A string or []byte can still carry anything, so
// TestModelSpan_NeverRecordsSecretsOrNumbers checks what the spans hold.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope of every span this package starts.
const tracerName = "github.com/iansmith/aatoolkit/driver"

// captureMessageContentEnv names the environment variable that turns message
// content capture on (see Config.TracerProvider).
const captureMessageContentEnv = "AATOOLKIT_CAPTURE_MESSAGE_CONTENT"

// The two span attributes the GenAI conventions have no key for.
var (
	// tierKey is the tier name the caller passed ("fast", "deep").
	tierKey = attribute.Key("aatoolkit.llm.tier")
	// timeToFirstContentKey is the seconds from sending the request to the
	// first non-empty content delta. It differs from the conventions'
	// time_to_first_chunk on upstreams that open with a role-only chunk and
	// on tiers that reason before they answer.
	timeToFirstContentKey = attribute.Key("aatoolkit.llm.time_to_first_content")
)

// The closed set of error.type values a model span can carry, besides an
// upstream's HTTP status code.
const (
	errTypeTimeout    = "timeout"
	errTypeCanceled   = "canceled"
	errTypeTransport  = "transport_error"
	errTypeStreamRead = "stream_read_error"
	errTypeUpstream   = "upstream_error"
	errTypeDecode     = "decode_error"
	errTypeEmpty      = "empty_response"
)

// errTypeOther is the conventions' fallback error.type, "_OTHER": what a span
// records when the call never reported how it ended, as when onSegment
// panics.
var errTypeOther = semconv.ErrorTypeOther.Value.AsString()

// tracerOptions and clientKind never change, so they are built once rather
// than allocated again on every request, traced or not.
var (
	tracerOptions = []trace.TracerOption{trace.WithSchemaURL(semconv.SchemaURL)}
	clientKind    = trace.WithSpanKind(trace.SpanKindClient)
)

// tracer returns the tracer for one model request: from tp, the configured
// provider, or when tp is nil from the global one as it is at this call. It
// is resolved per call and never cached, so a provider installed after New
// is used.
func tracer(tp trace.TracerProvider) trace.Tracer {
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(tracerName, tracerOptions...)
}

// serverAddrPort returns the host and port of rawURL for server.address and
// server.port: the hostname alone, never userinfo, path or query, and the
// explicit port, else the scheme's default. An empty addr or a zero port
// means the attribute is omitted.
func serverAddrPort(rawURL string) (addr string, port int) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", 0
	}
	addr = u.Hostname()
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return addr, 0
		}
		return addr, n
	}
	switch u.Scheme {
	case "https":
		return addr, 443
	case "http":
		return addr, 80
	}
	return addr, 0
}

// startModelSpan starts the CLIENT span for one chat-completions request
// with tracer(tp), a child of ctx's span, with the attributes known before it
// is sent. The returned context carries the span.
func startModelSpan(ctx context.Context, tp trace.TracerProvider, tier, model, rawURL string) (context.Context, *modelCall) {
	name := "chat"
	attrs := make([]attribute.KeyValue, 0, 7)
	attrs = append(attrs,
		semconv.GenAIOperationNameChat,
		// The wire format's flavour: every tier speaks OpenAI-compatible chat
		// completions. server.address tells the upstreams apart.
		semconv.GenAIProviderNameOpenAI,
		semconv.GenAIRequestStream(true),
		tierKey.String(tier),
	)
	if model != "" {
		name = "chat " + model
		attrs = append(attrs, semconv.GenAIRequestModel(model))
	}
	addr, port := serverAddrPort(rawURL)
	if addr != "" {
		attrs = append(attrs, semconv.ServerAddress(addr))
	}
	if port != 0 {
		attrs = append(attrs, semconv.ServerPort(port))
	}
	ctx, span := tracer(tp).Start(ctx, name, clientKind, trace.WithAttributes(attrs...))
	return ctx, &modelCall{span: span, recording: span.IsRecording()}
}

// modelCall is one request's span and the timings it gathers. It lives on
// the caller's goroutine for the length of one call; nothing shares it. When
// the span is not recording, every method but end is a no-op and end only
// ends the span: no clock reads, no JSON, no attributes.
type modelCall struct {
	span      trace.Span
	recording bool
	// issued is when the request was handed to the transport.
	issued time.Time
	// firstChunk and firstContent are measured from issued; sawChunk and
	// sawContent say whether each has been taken.
	firstChunk, firstContent time.Duration
	sawChunk, sawContent     bool
}

// requestSent marks the moment the request goes to the transport, which the
// first-arrival times are measured from.
func (c *modelCall) requestSent() {
	if c.recording {
		c.issued = time.Now()
	}
}

// now returns the current time while a first-arrival mark is still pending
// on a recording span, and the zero time otherwise, so a finished or
// non-recording call reads no clock per line.
func (c *modelCall) now() time.Time {
	if !c.recording || (c.sawChunk && c.sawContent) {
		return time.Time{}
	}
	return time.Now()
}

// observe takes the first-arrival marks a line just produced. hadChunk and
// hadContent are st's state before the line; at is the time read before the
// line was processed, so the onSegment calls it triggered are not counted.
func (c *modelCall) observe(st *streamState, hadChunk, hadContent bool, at time.Time) {
	if !c.recording {
		return
	}
	if !hadChunk && st.chunks > 0 {
		c.firstChunk, c.sawChunk = at.Sub(c.issued), true
	}
	if !hadContent && st.hasContent {
		c.firstContent, c.sawContent = at.Sub(c.issued), true
	}
}

// end records what the stream said about the response and ends the span.
// errType is "" on success; otherwise it becomes error.type and the status is
// Error with an empty description, because an error's text can carry an
// upstream body. Message content is recorded only when the capture variable
// is set, which is read only for a recording span.
func (c *modelCall) end(st *streamState, maxTokens int, contextWindow []byte, errType string) {
	defer c.span.End()
	if !c.recording {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 12)
	if maxTokens > 0 {
		attrs = append(attrs, semconv.GenAIRequestMaxTokens(maxTokens))
	}
	if st.model != "" {
		attrs = append(attrs, semconv.GenAIResponseModel(st.model))
	}
	if st.id != "" {
		attrs = append(attrs, semconv.GenAIResponseID(st.id))
	}
	finish := st.finishReason
	if finish == "" && errType != "" {
		finish = "error"
	}
	if finish != "" {
		attrs = append(attrs, semconv.GenAIResponseFinishReasons(finish))
	}
	if c.sawChunk {
		attrs = append(attrs, semconv.GenAIResponseTimeToFirstChunk(c.firstChunk.Seconds()))
	}
	if c.sawContent {
		attrs = append(attrs, timeToFirstContentKey.Float64(c.firstContent.Seconds()))
	}
	if st.usage != nil {
		attrs = append(attrs,
			semconv.GenAIUsageInputTokens(st.usage.PromptTokens),
			semconv.GenAIUsageOutputTokens(st.usage.CompletionTokens),
		)
	}
	if errType != "" {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errType))
		c.span.SetStatus(codes.Error, "")
	}
	if captureMessageContent() {
		if in, ok := genAIInputMessages(contextWindow); ok {
			attrs = append(attrs, semconv.GenAIInputMessagesKey.String(in))
		}
		if out, ok := genAIOutputMessages(st.contentBuf.String(), st.reasoningBuf.String(), finish); ok {
			attrs = append(attrs, semconv.GenAIOutputMessagesKey.String(out))
		}
	}
	c.span.SetAttributes(attrs...)
}

// captureMessageContent reports whether the capture variable is set to a
// value strconv.ParseBool reads as true. Unset, empty, false or unparsable
// is off.
func captureMessageContent() bool {
	on, err := strconv.ParseBool(os.Getenv(captureMessageContentEnv))
	return err == nil && on
}

// genAIPart and genAIMessage are the GenAI conventions' message shape for
// gen_ai.input.messages and gen_ai.output.messages.
type genAIPart struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type genAIMessage struct {
	Role         string      `json:"role"`
	Parts        []genAIPart `json:"parts"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// genAIInputMessages renders the request's messages array as GenAI messages:
// string content becomes one text part, array content keeps its text items,
// and null or absent content gives no parts. System messages stay here as
// role "system". ok is false when the window is not an array of objects.
func genAIInputMessages(contextWindow []byte) (string, bool) {
	var raw []json.RawMessage
	if err := json.Unmarshal(contextWindow, &raw); err != nil || raw == nil {
		return "", false
	}
	msgs := make([]genAIMessage, 0, len(raw))
	for _, r := range raw {
		if len(r) == 0 || r[0] != '{' {
			return "", false
		}
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(r, &m); err != nil {
			return "", false
		}
		msgs = append(msgs, genAIMessage{Role: m.Role, Parts: contentParts(m.Content)})
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// contentParts turns one message's content into text parts: a string is one
// part; an array keeps its {"type":"text"} items and drops the rest; anything
// else, null included, gives none. The result is never nil, so it encodes as
// [].
func contentParts(content json.RawMessage) []genAIPart {
	parts := []genAIPart{}
	if len(content) == 0 {
		return parts
	}
	switch content[0] {
	case '"':
		var s string
		if err := json.Unmarshal(content, &s); err == nil {
			parts = append(parts, genAIPart{Type: "text", Content: s})
		}
	case '[':
		var items []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(content, &items); err == nil {
			for _, it := range items {
				if it.Type == "text" {
					parts = append(parts, genAIPart{Type: "text", Content: it.Text})
				}
			}
		}
	}
	return parts
}

// genAIOutputMessages renders the model's output as one assistant GenAI
// message: a reasoning part and a text part, each only when non-empty, and
// the finish reason when there is one. ok is false when there is no output
// at all; none is ever invented.
func genAIOutputMessages(content, reasoning, finishReason string) (string, bool) {
	if content == "" && reasoning == "" {
		return "", false
	}
	parts := make([]genAIPart, 0, 2)
	if reasoning != "" {
		parts = append(parts, genAIPart{Type: "reasoning", Content: reasoning})
	}
	if content != "" {
		parts = append(parts, genAIPart{Type: "text", Content: content})
	}
	b, err := json.Marshal([]genAIMessage{{Role: "assistant", Parts: parts, FinishReason: finishReason}})
	if err != nil {
		return "", false
	}
	return string(b), true
}
