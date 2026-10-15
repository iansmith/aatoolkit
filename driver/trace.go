package driver

// ---------------------------------------------------------------------------
// driver: OpenTelemetry spans for model requests (AATK-159). This is the only
// file that starts, annotates or ends a span.
// ---------------------------------------------------------------------------

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
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
	tierKey               = attribute.Key("aatoolkit.llm.tier")
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

// errTypeOther is the conventions' fallback error.type, "_OTHER".
var errTypeOther = semconv.ErrorTypeOther.Value.AsString()

// modelCall is the per-request span state.
type modelCall struct {
	span                     trace.Span
	recording                bool
	issued                   time.Time
	firstChunk, firstContent time.Duration
	sawChunk, sawContent     bool
}

// startModelSpan is a stub.
func (h *Host) startModelSpan(ctx context.Context, tier, model, rawURL string) (context.Context, *modelCall) {
	return ctx, &modelCall{}
}

// requestSent is a stub.
func (c *modelCall) requestSent() {}

// now is a stub.
func (c *modelCall) now() time.Time { return time.Time{} }

// observe is a stub.
func (c *modelCall) observe(st *streamState, hadChunk, hadContent bool, at time.Time) {}

// end is a stub.
func (c *modelCall) end(st *streamState, maxTokens int, contextWindow []byte, errType string) {}

// serverAddrPort is a stub.
func serverAddrPort(rawURL string) (addr string, port int) { return "", 0 }
