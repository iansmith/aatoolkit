package driver

import (
	"testing"
	"time"
)

// BenchmarkSendStream_FirstSegment measures the time from a SendStream call to
// its first onSegment callback, reported as ns/first-seg. The upstream is a
// local scripted server with no delays: a role-only chunk, one content chunk
// long enough to end in a segment boundary, a finish chunk, then [DONE]. The
// Host is a struct literal (no tracer provider) whose client keeps connections
// alive, so the metric is the send path's own cost to the first segment.
//
// Each iteration makes a loopback request: never run this in a parallel
// -count stress loop, which exhausts loopback ports.
func BenchmarkSendStream_FirstSegment(b *testing.B) {
	srv, _ := newScriptedSSEServer(
		sseStep{line: `data: {"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`},
		sseStep{line: `data: {"choices":[{"delta":{"content":"The quick brown fox jumps over the lazy dog."},"finish_reason":null}]}`},
		sseStep{line: `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`},
		sseStep{line: `data: [DONE]`},
	)
	defer srv.Close()
	h := &Host{
		client: srv.Client(),
		tiers: map[string]Tier{
			"fast": {URL: srv.URL + "/v1/chat/completions", Model: "test", MaxTokens: 512},
		},
	}
	window := []byte(`[{"role":"user","content":"hello"}]`)

	var total time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var first time.Duration
		fired := false
		start := time.Now()
		_, _, err := h.SendStream(window, "fast", func(string) {
			if !fired {
				first, fired = time.Since(start), true
			}
		})
		if err != nil {
			b.Fatalf("SendStream: %v", err)
		}
		if !fired {
			b.Fatal("onSegment never fired")
		}
		total += first
	}
	b.ReportMetric(float64(total.Nanoseconds())/float64(b.N), "ns/first-seg")
}
