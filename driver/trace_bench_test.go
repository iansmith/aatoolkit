package driver

import (
	"context"
	"testing"
)

// BenchmarkModelSpan_NoProvider measures what tracing adds to one model
// request when no tracer provider is configured or installed: starting the
// span with its start attributes, the timing hooks, and ending it. It makes no
// network call. It never installs a global provider, so it runs against the
// no-op default.
func BenchmarkModelSpan_NoProvider(b *testing.B) {
	h := &Host{}
	st := &streamState{acc: newSegmentAccumulator(minSegmentChars)}
	window := []byte(`[{"role":"user","content":"hello"}]`)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, call := h.startModelSpan(ctx, "fast", "test", "http://127.0.0.1:8080/v1/chat/completions")
		call.requestSent()
		call.observe(st, false, false, call.now())
		call.end(st, 512, window, "")
	}
}
