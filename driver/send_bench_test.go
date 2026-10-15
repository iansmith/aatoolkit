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
	benchFirstSegment(b,
		sseStep{line: `data: {"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`},
		sseStep{line: `data: {"choices":[{"delta":{"content":"The quick brown fox jumps over the lazy dog."},"finish_reason":null}]}`},
		sseStep{line: `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`},
		sseStep{line: `data: [DONE]`},
	)
}

// BenchmarkSendStream_FirstSegmentHostedChunks is BenchmarkSendStream_FirstSegment
// with every chunk shaped as a hosted OpenAI-compatible upstream sends it: an
// id and a model on each, "usage": null on all but a final choice-less usage
// chunk. The same caution about stress loops applies.
func BenchmarkSendStream_FirstSegmentHostedChunks(b *testing.B) {
	const meta = `"id":"chatcmpl-0a1b2c3d4e5f","object":"chat.completion.chunk","created":1760000000,"model":"vendor/some-model-v1",`
	benchFirstSegment(b,
		sseStep{line: `data: {` + meta + `"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`},
		sseStep{line: `data: {` + meta + `"choices":[{"index":0,"delta":{"content":"The quick brown fox jumps over the lazy dog."},"finish_reason":null}],"usage":null}`},
		sseStep{line: `data: {` + meta + `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`},
		sseStep{line: `data: {` + meta + `"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21}}`},
		sseStep{line: `data: [DONE]`},
	)
}

// benchFirstSegment runs the first-segment measurement against a scripted
// upstream replaying steps.
func benchFirstSegment(b *testing.B, steps ...sseStep) {
	srv, _ := newScriptedSSEServer(steps...)
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
