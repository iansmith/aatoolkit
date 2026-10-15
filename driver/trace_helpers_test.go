package driver

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newRecorder returns an SDK tracer provider that records every span, and the
// recorder holding them. The provider is shut down when the test ends.
func newRecorder(t *testing.T) (*tracetest.SpanRecorder, trace.TracerProvider) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec, tp
}

// tracedHost builds a Host through New whose "fast" tier is tier with its URL
// set to url, tracing into a fresh recorder. A test that needs a caller span
// starts it from h.tracerProvider, so the caller and the model span share the
// recorder.
func tracedHost(t *testing.T, url string, tier Tier) (*Host, *tracetest.SpanRecorder) {
	t.Helper()
	rec, tp := newRecorder(t)
	tier.URL = url
	h := New(Config{
		Tiers:          map[string]Tier{"fast": tier},
		Prompt:         func() string { return "" },
		TracerProvider: tp,
	})
	return h, rec
}

// modelSpans returns the ended spans that are model-request spans: the ones
// carrying gen_ai.operation.name. A caller span a test started is not one.
func modelSpans(rec *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			if string(kv.Key) == "gen_ai.operation.name" {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// onlyModelSpan fails the test unless rec holds exactly one model span, and
// returns it.
func onlyModelSpan(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := modelSpans(rec)
	if len(spans) != 1 {
		t.Fatalf("got %d model spans, want exactly 1", len(spans))
	}
	return spans[0]
}

// attrMap returns span's attributes keyed by their literal key string.
func attrMap(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := make(map[string]attribute.Value, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

// withCaptureEnv sets the content-capture environment variable to v for the
// rest of the test. t.Setenv forbids t.Parallel in the calling test.
func withCaptureEnv(t *testing.T, v string) {
	t.Helper()
	t.Setenv(captureMessageContentEnv, v)
}

// withoutCaptureEnv unsets the content-capture environment variable for the
// rest of the test, whatever the shell running the tests set it to. t.Setenv
// records the value to restore; the Unsetenv then removes it.
func withoutCaptureEnv(t *testing.T) {
	t.Helper()
	t.Setenv(captureMessageContentEnv, "")
	if err := os.Unsetenv(captureMessageContentEnv); err != nil {
		t.Fatalf("unsetting %s: %v", captureMessageContentEnv, err)
	}
}

// scanSpans looks through everything a span exports -- its name, every
// attribute key and value (each element of a slice value), its status
// description, and its events' and links' names and attributes -- for any of
// needles (case-insensitively) or a match of re (nil skips the regexp). It
// returns one description per hit.
func scanSpans(spans []sdktrace.ReadOnlySpan, needles []string, re *regexp.Regexp) []string {
	var hits []string
	check := func(span sdktrace.ReadOnlySpan, where, s string) {
		lower := strings.ToLower(s)
		for _, n := range needles {
			if strings.Contains(lower, strings.ToLower(n)) {
				hits = append(hits, fmt.Sprintf("span %q %s holds %q: %q", span.Name(), where, n, s))
			}
		}
		if re != nil {
			if m := re.FindString(s); m != "" {
				hits = append(hits, fmt.Sprintf("span %q %s matches %v (%q): %q", span.Name(), where, re, m, s))
			}
		}
	}
	checkAttrs := func(span sdktrace.ReadOnlySpan, where string, kvs []attribute.KeyValue) {
		for _, kv := range kvs {
			check(span, where+" key", string(kv.Key))
			for _, v := range attrStrings(kv.Value) {
				check(span, where+" "+string(kv.Key), v)
			}
		}
	}
	for _, span := range spans {
		check(span, "name", span.Name())
		checkAttrs(span, "attribute", span.Attributes())
		check(span, "status description", span.Status().Description)
		for _, ev := range span.Events() {
			check(span, "event name", ev.Name)
			checkAttrs(span, "event attribute", ev.Attributes)
		}
		for _, l := range span.Links() {
			checkAttrs(span, "link attribute", l.Attributes)
		}
	}
	return hits
}

// attrStrings renders an attribute value as the strings it carries: each
// element of a string slice, or the value's one rendering otherwise.
func attrStrings(v attribute.Value) []string {
	if v.Type() == attribute.STRINGSLICE {
		return v.AsStringSlice()
	}
	return []string{v.Emit()}
}
