package driver

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestTierIncludeUsage_AddsStreamOptions: a tier with IncludeUsage sends
// "stream_options": {"include_usage": true}; a tier without it sends no
// stream_options key, and every other key is byte-equal between the two.
func TestTierIncludeUsage_AddsStreamOptions(t *testing.T) {
	srv, reqs := newScriptedSSEServer(
		sseStep{line: `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`},
		sseStep{line: `data: [DONE]`},
	)
	defer srv.Close()
	send := func(includeUsage bool) map[string]json.RawMessage {
		t.Helper()
		h := New(Config{
			Tiers: map[string]Tier{"fast": {
				URL: srv.URL + "/v1/chat/completions", Model: "test", MaxTokens: 8, IncludeUsage: includeUsage,
			}},
			Prompt: func() string { return "" },
		})
		if _, _, err := h.Send([]byte(`[{"role":"user","content":"hi"}]`), "fast"); err != nil {
			t.Fatalf("Send (IncludeUsage %v): %v", includeUsage, err)
		}
		body := reqs.LastBody()
		if body == nil {
			t.Fatalf("Send (IncludeUsage %v): no JSON request body recorded", includeUsage)
		}
		return body
	}

	on := send(true)
	var compact bytes.Buffer
	if err := json.Compact(&compact, on["stream_options"]); err != nil || compact.String() != `{"include_usage":true}` {
		t.Errorf("stream_options with IncludeUsage = %s (compact err %v), want {\"include_usage\":true}", on["stream_options"], err)
	}

	off := send(false)
	if got, ok := off["stream_options"]; ok {
		t.Errorf("stream_options = %s with IncludeUsage unset; the default request must not change", got)
	}
	delete(on, "stream_options")
	if len(on) != len(off) {
		t.Errorf("flag-on body has %d other keys, flag-off body has %d", len(on), len(off))
	}
	for k, v := range off {
		if !bytes.Equal(on[k], v) {
			t.Errorf("key %q differs: flag on %s, flag off %s", k, on[k], v)
		}
	}
}

// TestDecodeSSELine_ChoicelessChunkCarriesUsage: a data chunk with no choices
// is still decoded -- it is how a hosted upstream delivers the final usage --
// and every chunk's model, id and usage ride beside its delta.
func TestDecodeSSELine_ChoicelessChunkCarriesUsage(t *testing.T) {
	t.Run("usage chunk without choices", func(t *testing.T) {
		d, ok, err := decodeSSELine(`data: {"id":"r1","model":"served-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}`)
		if err != nil || !ok {
			t.Fatalf("decodeSSELine = ok %v, err %v; want a decoded chunk", ok, err)
		}
		if d.usage == nil || *d.usage != (sseUsage{PromptTokens: 10, CompletionTokens: 8}) {
			t.Errorf("usage = %+v, want {PromptTokens:10 CompletionTokens:8}", d.usage)
		}
		if d.model != "served-model" || d.id != "r1" {
			t.Errorf("model, id = %q, %q; want served-model, r1", d.model, d.id)
		}
		if d.Content != "" || d.reasoningText() != "" || d.finishReason != "" {
			t.Errorf("choice-less chunk produced text or a finish reason: %+v", d)
		}
	})

	t.Run("null usage beside a delta", func(t *testing.T) {
		d, ok, err := decodeSSELine(`data: {"id":"r1","model":"served-model","choices":[{"delta":{"content":"Hi"},"finish_reason":null}],"usage":null}`)
		if err != nil || !ok {
			t.Fatalf("decodeSSELine = ok %v, err %v; want a decoded chunk", ok, err)
		}
		if d.Content != "Hi" || d.usage != nil || d.model != "served-model" || d.id != "r1" {
			t.Errorf("delta = %+v (usage %v); want content Hi, no usage, model served-model, id r1", d, d.usage)
		}
	})

	t.Run("lines with nothing to decode", func(t *testing.T) {
		for _, line := range []string{"", ": keep-alive", "event: message", "data: [DONE]"} {
			if _, ok, err := decodeSSELine(line); ok || err != nil {
				t.Errorf("decodeSSELine(%q) = ok %v, err %v; want ok false, no error", line, ok, err)
			}
		}
	})
}

// TestProcessSSELine_UsageChunkKeepsFinishReason: folding a stream into
// streamState counts every decoded chunk, keeps the first non-empty model and
// id and the last non-nil usage, and a choice-less usage chunk after the
// finish chunk leaves the content and the finish reason the earlier chunks
// set. The trailing chunk with "usage": null must not clear the usage.
func TestProcessSSELine_UsageChunkKeepsFinishReason(t *testing.T) {
	st := &streamState{acc: newSegmentAccumulator(minSegmentChars)}
	lines := []string{
		`data: {"id":"","model":"","choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`,
		`data: {"id":"r1","model":"m1","choices":[{"delta":{"content":"Hello"},"finish_reason":null}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`data: {"id":"r2","model":"m2","choices":[{"delta":{},"finish_reason":"length"}],"usage":null}`,
		`data: {"id":"r2","model":"m2","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}`,
		`data: {"id":"r3","model":"m3","choices":[],"usage":null}`,
		``,
		`data: [DONE]`,
	}
	for _, line := range lines {
		if err := processSSELine(line, st, func(string) {}); err != nil {
			t.Fatalf("processSSELine(%q): %v", line, err)
		}
	}
	if st.chunks != 5 {
		t.Errorf("chunks = %d, want 5 (blank and [DONE] lines are not chunks)", st.chunks)
	}
	if st.model != "m1" || st.id != "r1" {
		t.Errorf("model, id = %q, %q; want the first non-empty: m1, r1", st.model, st.id)
	}
	if st.usage == nil || *st.usage != (sseUsage{PromptTokens: 10, CompletionTokens: 8}) {
		t.Errorf("usage = %+v, want the last non-nil one: {PromptTokens:10 CompletionTokens:8}", st.usage)
	}
	if st.finishReason != "length" {
		t.Errorf("finishReason = %q; the usage chunk must not overwrite length", st.finishReason)
	}
	if got := st.contentBuf.String(); got != "Hello" || !st.hasContent {
		t.Errorf("content = %q (hasContent %v), want Hello", got, st.hasContent)
	}
}

// TestSendStream_UsageOnlyStreamIsStillEmpty: decoding choice-less chunks does
// not make a stream with usage but no content succeed; its error text is
// today's, with and without a finish reason.
func TestSendStream_UsageOnlyStreamIsStillEmpty(t *testing.T) {
	cases := []struct {
		name    string
		steps   []sseStep
		wantErr string
	}{
		{
			name: "no finish reason",
			steps: []sseStep{
				{line: `data: {"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`},
				{line: `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":0}}`},
				{line: `data: [DONE]`},
			},
			wantErr: "no content in SSE stream",
		},
		{
			name: "finish reason length",
			steps: []sseStep{
				{line: `data: {"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`},
				{line: `data: {"choices":[{"delta":{},"finish_reason":"length"}],"usage":null}`},
				{line: `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":8}}`},
				{line: `data: [DONE]`},
			},
			wantErr: "no content in SSE stream (finish_reason length)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newScriptedSSEServer(tc.steps...)
			defer srv.Close()
			h := New(Config{
				Tiers:  map[string]Tier{"fast": {URL: srv.URL + "/v1/chat/completions", Model: "test", MaxTokens: 8}},
				Prompt: func() string { return "" },
			})
			content, _, err := h.SendStream([]byte(`[]`), "fast", func(string) {})
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("SendStream = content %q, err %v; want error %q", content, err, tc.wantErr)
			}
		})
	}
}

// TestDecodeSSELine_OddMetadataNeverFailsTheChunk: a chunk's id, model and
// usage feed only the span, so a value of an unexpected type in one of them
// is dropped, never half read, and the chunk decodes as it did before those
// fields were read at all. Its content comes through, and the well-typed
// fields beside the odd one are kept.
func TestDecodeSSELine_OddMetadataNeverFailsTheChunk(t *testing.T) {
	cases := []struct {
		name              string
		line              string
		wantModel, wantID string
		wantUsage         *sseUsage
	}{
		{
			name:      "numeric id",
			line:      `data: {"id":7,"model":"m","choices":[{"delta":{"content":"hi"}}]}`,
			wantModel: "m",
		},
		{
			name:   "array model",
			line:   `data: {"id":"r1","model":["m"],"choices":[{"delta":{"content":"hi"}}]}`,
			wantID: "r1",
		},
		{
			name:      "fractional token count",
			line:      `data: {"id":"r1","model":"m","choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":1.5,"completion_tokens":8}}`,
			wantModel: "m", wantID: "r1",
		},
		{
			name: "usage that is not an object",
			line: `data: {"choices":[{"delta":{"content":"hi"}}],"usage":"n/a"}`,
		},
		{
			name:      "odd id and model beside a good usage",
			line:      `data: {"id":{"v":1},"model":true,"choices":[{"delta":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
			wantUsage: &sseUsage{PromptTokens: 3, CompletionTokens: 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok, err := decodeSSELine(tc.line)
			if err != nil || !ok {
				t.Fatalf("decodeSSELine = ok %v, err %v; want the chunk decoded", ok, err)
			}
			if d.Content != "hi" {
				t.Errorf("content = %q, want hi", d.Content)
			}
			if d.model != tc.wantModel || d.id != tc.wantID {
				t.Errorf("model, id = %q, %q; want %q, %q", d.model, d.id, tc.wantModel, tc.wantID)
			}
			if (d.usage == nil) != (tc.wantUsage == nil) || (d.usage != nil && *d.usage != *tc.wantUsage) {
				t.Errorf("usage = %+v, want %+v", d.usage, tc.wantUsage)
			}
		})
	}
}

// TestSendStream_OddMetadataStillSucceeds: a stream whose chunks carry an id,
// model or usage of an unexpected type answers as it did before tracing
// existed, with no provider and with one, and the span leaves the odd values
// out rather than failing the call.
func TestSendStream_OddMetadataStillSucceeds(t *testing.T) {
	steps := []sseStep{
		{line: `data: {"id":7,"model":"served-model","choices":[{"delta":{"content":"The quick brown fox jumps over the lazy dog."},"finish_reason":null}],"usage":null}`},
		{line: `data: {"id":7,"model":"served-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":"n/a"}`},
		{line: `data: {"id":7,"model":["served-model"],"choices":[],"usage":{"prompt_tokens":1.5,"completion_tokens":8}}`},
		{line: `data: [DONE]`},
	}
	const want = "The quick brown fox jumps over the lazy dog."

	t.Run("no provider", func(t *testing.T) {
		srv, _ := newScriptedSSEServer(steps...)
		defer srv.Close()
		h := &Host{client: srv.Client(), tiers: map[string]Tier{"fast": {URL: chatURL(srv), Model: "test", MaxTokens: 512}}}
		content, _, err := h.SendStream(traceWindow, "fast", func(string) {})
		if err != nil || string(content) != want {
			t.Fatalf("SendStream = %q, %v; want %q, no error", content, err, want)
		}
	})

	t.Run("traced", func(t *testing.T) {
		withoutCaptureEnv(t)
		srv, _ := newScriptedSSEServer(steps...)
		defer srv.Close()
		h, rec := tracedHost(t, chatURL(srv), Tier{Model: "test", MaxTokens: 512})
		content, _, err := h.SendStream(traceWindow, "fast", func(string) {})
		if err != nil || string(content) != want {
			t.Fatalf("SendStream = %q, %v; want %q, no error", content, err, want)
		}
		span := onlyModelSpan(t, rec)
		requireAttrs(t, span, map[string]any{
			"gen_ai.response.model":          "served-model",
			"gen_ai.response.finish_reasons": []string{"stop"},
		})
		requireNoAttrs(t, span, "gen_ai.response.id", "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "error.type")
	})
}

// hostedChunk is a content chunk as an OpenAI-compatible upstream sends one:
// an id and a model on every chunk, and "usage": null on every chunk but the
// last. bareChunk is the same chunk without those three fields, which is what
// decoding it costs when they are not read at all.
const (
	hostedChunk = `data: {"id":"chatcmpl-0a1b2c3d4e5f","object":"chat.completion.chunk","created":1760000000,"model":"vendor/some-model-v1",` +
		`"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}],"usage":null}`
	bareChunk = `data: {"object":"chat.completion.chunk","created":1760000000,` +
		`"choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`
)

// TestDecodeSSELine_MetadataCostsOneAllocationEach: reading a chunk's id and
// model costs one allocation each, the string itself, and a null usage costs
// none, so a traced or untraced stream pays no more per chunk than that for
// the span's metadata.
func TestDecodeSSELine_MetadataCostsOneAllocationEach(t *testing.T) {
	allocs := func(line string) float64 {
		return testing.AllocsPerRun(200, func() {
			if _, ok, err := decodeSSELine(line); !ok || err != nil {
				t.Fatalf("decodeSSELine(%q) = ok %v, err %v", line, ok, err)
			}
		})
	}
	bare, hosted := allocs(bareChunk), allocs(hostedChunk)
	if hosted > bare+2 {
		t.Errorf("a chunk with an id, a model and a null usage took %v allocs, the same chunk without them %v; "+
			"want at most 2 more, one per string", hosted, bare)
	}
}

// TestDecodeSSELine_MetadataStringsDecodeAsJSON: a chunk's id and model come
// back exactly as encoding/json decodes the same string literal, escapes and
// invalid UTF-8 included.
func TestDecodeSSELine_MetadataStringsDecodeAsJSON(t *testing.T) {
	literals := []string{
		`""`,
		`"plain-model-v1"`,
		`"vendor\/model"`,
		`"tab\there"`,
		`"café 😀"`,
		"\"caf\xc3\xa9 raw\"",
		"\"bad \xff byte\"",
		"\"cut \xe2\x82 rune\"",
	}
	for _, lit := range literals {
		var want string
		if err := json.Unmarshal([]byte(lit), &want); err != nil {
			t.Fatalf("encoding/json rejects %q: %v", lit, err)
		}
		d, ok, err := decodeSSELine(`data: {"id":` + lit + `,"model":` + lit + `,"choices":[{"delta":{"content":"hi"}}]}`)
		if err != nil || !ok {
			t.Fatalf("decodeSSELine with %q = ok %v, err %v", lit, ok, err)
		}
		if d.id != want || d.model != want {
			t.Errorf("literal %q: id %q, model %q; want %q for both", lit, d.id, d.model, want)
		}
	}
}

// BenchmarkDecodeSSELine_HostedChunk measures decoding one hostedChunk, the
// per-chunk cost a real stream pays whether or not a span records it.
func BenchmarkDecodeSSELine_HostedChunk(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok, err := decodeSSELine(hostedChunk); !ok || err != nil {
			b.Fatalf("decodeSSELine = ok %v, err %v", ok, err)
		}
	}
}
