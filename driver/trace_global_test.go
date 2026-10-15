package driver

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// globalTPEnv marks the child process TestTracerProvider_NilUsesGlobalAtCallTime
// re-executes; only that child installs a global provider or propagator.
const globalTPEnv = "AATOOLKIT_TEST_GLOBAL_TP"

// TestTracerProvider_NilUsesGlobalAtCallTime: a Host built with no
// TracerProvider uses whichever global provider is installed when each call
// is made -- including one installed after New, and a later replacement --
// and never puts trace context or baggage on the upstream request, even with
// a global propagator installed. Setting the global provider would leak into
// every other test in this binary, so the test re-executes itself and makes
// the global changes only in that child.
func TestTracerProvider_NilUsesGlobalAtCallTime(t *testing.T) {
	if os.Getenv(globalTPEnv) == "1" {
		globalProviderChild(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTracerProvider_NilUsesGlobalAtCallTime$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), globalTPEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestTracerProvider_NilUsesGlobalAtCallTime") {
		t.Fatalf("child process did not run the test:\n%s", out)
	}
}

// globalProviderChild is the child process's half of
// TestTracerProvider_NilUsesGlobalAtCallTime.
func globalProviderChild(t *testing.T) {
	srv, reqs := newScriptedSSEServer(okSteps()...)
	defer srv.Close()
	// Built before any global provider exists.
	h := New(Config{
		Tiers:  map[string]Tier{"fast": {URL: chatURL(srv), Model: "test", MaxTokens: 512}},
		Prompt: func() string { return "" },
	})
	send := func(ctx context.Context) {
		t.Helper()
		if _, _, err := h.SendStreamContext(ctx, traceWindow, "fast", func(string) {}); err != nil {
			t.Fatalf("SendStreamContext: %v", err)
		}
	}
	install := func() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
		rec := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
		otel.SetTracerProvider(tp)
		return rec, tp
	}

	recA, _ := install()
	send(context.Background())
	if n := len(modelSpans(recA)); n != 1 {
		t.Fatalf("provider A installed after New: %d model spans in A, want 1", n)
	}

	recB, tpB := install()
	send(context.Background())
	if a, b := len(modelSpans(recA)), len(modelSpans(recB)); a != 1 || b != 1 {
		t.Fatalf("after replacing A with B: %d model spans in A, %d in B; want 1 and 1", a, b)
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	member, err := baggage.NewMember("tenant", "acme")
	if err != nil {
		t.Fatalf("baggage member: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage: %v", err)
	}
	ctx, caller := tpB.Tracer("test").Start(baggage.ContextWithBaggage(context.Background(), bag), "caller")
	defer caller.End()
	// The installed propagator has something to inject from ctx, so the
	// header check below is not vacuous.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if carrier.Get("traceparent") == "" || carrier.Get("baggage") == "" {
		t.Fatalf("the global propagator injects %v from the caller's context; want traceparent and baggage", carrier)
	}
	send(ctx)
	hdr := reqs.LastHeader()
	for _, k := range []string{"traceparent", "tracestate", "baggage"} {
		if v := hdr.Get(k); v != "" {
			t.Errorf("upstream request carried %s: %q; the engine must not propagate", k, v)
		}
	}
}
