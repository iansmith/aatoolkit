package driver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A long clip (well over the short-clip floor) must STILL get a trailing
// silence pad — the bug was that long clips were returned unchanged, so afplay
// clipped the final word ("recommendations" → "recommen"). Short clips are
// additionally floored up to minSeconds.
func TestPadWAVSilenceAlwaysAppendsTail(t *testing.T) {
	const byteRate = 16000 * 2 // buildWAV16kMono: 16kHz mono 16-bit PCM

	longPCM := make([]byte, 5*byteRate) // 5s — already past the 4s floor
	long := buildWAV16kMono(longPCM)
	before, _ := wavData(long)
	after, _ := wavData(padWAVSilence(long))
	if grew := len(after) - len(before); grew < byteRate*3/4 {
		t.Fatalf("long clip gained %d bytes of tail, want ~%d (~1s of silence)", grew, byteRate)
	}

	shortPCM := make([]byte, byteRate/2) // 0.5s
	short := buildWAV16kMono(shortPCM)
	if sp, _ := wavData(padWAVSilence(short)); len(sp) < 4*byteRate {
		t.Fatalf("short clip data = %d bytes, want >= %d (4s floor)", len(sp), 4*byteRate)
	}
}

// ---------------------------------------------------------------------------
// Phase 0 — Send() streaming
// ---------------------------------------------------------------------------

// sseServer builds a httptest.Server that responds with SSE-formatted lines.
func sseServer(lines ...string) *httptest.Server {
	body := strings.Join(lines, "\n")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
}

// makeHost returns an Host configured to hit the given httptest server URL
// at the "fast" tier's completions Tier.
func makeHost(srv *httptest.Server) *Host {
	return &Host{
		client: srv.Client(),
		tiers: map[string]Tier{
			"fast": {URL: srv.URL + "/v1/chat/completions", Model: "test", Reasoning: false, MaxTokens: 512},
			"deep": {URL: "http://127.0.0.1:1", Model: "test", Reasoning: true, MaxTokens: 4096},
		},
	}
}

// Send() must parse SSE data lines, accumulate content from every delta, and
// return the same shape as the old non-streaming path. SSE also carries
// reasoning_content on reasoning tiers.
func TestSendStreamsAndAccumulates(t *testing.T) {
	srv := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}")
	defer srv.Close()

	h := makeHost(srv)
	content, reasoning, err := h.Send([]byte("[]"), "fast")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(content) != "Hello" {
		t.Fatalf("want content 'Hello', got %q", content)
	}
	if len(reasoning) != 0 {
		t.Fatalf("non-reasoning tier returned Reasoning: %q", reasoning)
	}
}

// SSE responses may have blank lines between data: lines; Send() must skip them.
func TestSendSkipsBlankSSELines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"World\"},\"finish_reason\":null}]}\n\n")
	}))
	defer srv.Close()

	h := makeHost(srv)
	content, _, err := h.Send([]byte("[]"), "fast")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(content) != "HelloWorld" {
		t.Fatalf("want 'HelloWorld', got %q", content)
	}
}

// SSE responses may end with a data: [DONE] line; Send() must skip it.
func TestSendSkipsDoneMarker(t *testing.T) {
	srv := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n",
		"data: [DONE]\n",
	)
	defer srv.Close()

	h := makeHost(srv)
	content, _, err := h.Send([]byte("[]"), "fast")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(content) != "Hello" {
		t.Fatalf("want 'Hello', got %q", content)
	}
}

// A chunked SSE stream must accumulate content across all data: lines.
func TestSendAccumulatesMultiChunk(t *testing.T) {
	srv := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"World\"},\"finish_reason\":null}]}\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"!\"},\"finish_reason\":null}]}\n",
	)
	defer srv.Close()

	h := makeHost(srv)
	content, _, err := h.Send([]byte("[]"), "fast")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(content) != "HelloWorld!" {
		t.Fatalf("want 'HelloWorld!', got %q", content)
	}
}

// Reasoning-tier responses carry reasoning_content alongside content.
func TestSendReasoningContent(t *testing.T) {
	srv := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\",\"reasoning_content\":\"because\"},\"finish_reason\":null}]}")
	defer srv.Close()

	h := makeHost(srv)
	content, reasoning, err := h.Send([]byte("[]"), "fast")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(content) != "answer" {
		t.Fatalf("content: want 'answer', got %q", content)
	}
	if string(reasoning) != "because" {
		t.Fatalf("Reasoning: want 'because', got %q", reasoning)
	}
}

// Unknown tier must still error with a clear message.
func TestSendUnknownTier(t *testing.T) {
	srv := sseServer()
	defer srv.Close()
	h := makeHost(srv)
	_, _, err := h.Send([]byte("[]"), "bogus")
	if err == nil {
		t.Fatal("expected error for unknown tier, got nil")
	}
}

// ---------------------------------------------------------------------------
// Phase 0 — SendStream() segmentation
// ---------------------------------------------------------------------------

// SendStream must call onSegment at each punctuation boundary (. ? ! , \n) and
// return the full assembled text when done.
func TestSendStreamSegmentsAndReturnsFull(t *testing.T) {
	srv := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello, \"},\"finish_reason\":null}]}")
	defer srv.Close()

	h := makeHost(srv)
	var segments []string
	full, _, err := h.SendStream([]byte("[]"), "fast", func(s string) {
		segments = append(segments, s)
	})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	// "Hello, " is 7 chars — below the 30-char mid-stream threshold so it won't
	// flush at the comma. It IS flushed at end-of-stream so no text is dropped.
	if len(segments) != 1 {
		t.Fatalf("want 1 segment (end-of-stream flush), got %d: %v", len(segments), segments)
	}
	if segments[0] != "Hello, " {
		t.Fatalf("segment mismatch: %q", segments[0])
	}
	if string(full) != "Hello, " {
		t.Fatalf("want full 'Hello, ', got %q", full)
	}
}

// A stream that crosses the 30-char threshold must flush the segment at the
// punctuation boundary and start a new buffer.
func TestSendStreamFlushesAtPunctuationAboveThreshold(t *testing.T) {
	// 36 chars of text ending with a period — should flush at the period.
	srv := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"The quick brown fox jumps over the lazy dog. \"},\"finish_reason\":null}]}")
	defer srv.Close()

	h := makeHost(srv)
	var segments []string
	full, _, err := h.SendStream([]byte("[]"), "fast", func(s string) {
		segments = append(segments, s)
	})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("want 1 segment, got %d: %v", len(segments), segments)
	}
	if segments[0] != "The quick brown fox jumps over the lazy dog." {
		t.Fatalf("segment mismatch: %q", segments[0])
	}
	if string(full) != "The quick brown fox jumps over the lazy dog. " {
		t.Fatalf("full mismatch: %q", full)
	}
}

// Multi-segment stream: each boundary above 30 chars triggers a flush.
func TestSendStreamMultiSegment(t *testing.T) {
	srv := sseServer(
		"data: {\"choices\":[{\"delta\":{\"content\":\"This is the first sentence. \"},\"finish_reason\":null}]}")
	defer srv.Close()

	h := makeHost(srv)
	var segments []string
	full, _, err := h.SendStream([]byte("[]"), "fast", func(s string) {
		segments = append(segments, s)
	})
	if err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	// "This is the first sentence. " has 26 chars before the period — below the
	// 30-char mid-stream threshold, so the period doesn't trigger a flush. The
	// full text IS flushed at end-of-stream (non-whitespace content).
	if len(segments) != 1 {
		t.Fatalf("want 1 segment (end-of-stream flush), got %d: %v", len(segments), segments)
	}
	if segments[0] != "This is the first sentence. " {
		t.Fatalf("segment mismatch: %q", segments[0])
	}
	if string(full) != "This is the first sentence. " {
		t.Fatalf("full mismatch: %q", full)
	}
}

// Unknown tier errors.
func TestSendStreamUnknownTier(t *testing.T) {
	srv := sseServer()
	defer srv.Close()
	h := makeHost(srv)
	_, _, err := h.SendStream([]byte("[]"), "bogus", func(s string) {})
	if err == nil {
		t.Fatal("expected error for unknown tier, got nil")
	}
}

// ParseStreamScheme is the -stream-scheme flag surface (PRD D15): it must
// default to "wss" when omitted, accept "ws"/"wss" explicitly, and reject any
// other value at flag-parse time so an invalid scheme never reaches
// twilio.Server.StreamScheme.
func TestParseStreamScheme_DefaultsToWss(t *testing.T) {
	got, err := ParseStreamScheme(nil)
	if err != nil {
		t.Fatalf("ParseStreamScheme(nil): unexpected error: %v", err)
	}
	if got != "wss" {
		t.Fatalf("ParseStreamScheme(nil) = %q, want %q", got, "wss")
	}
}

func TestParseStreamScheme_AcceptsExplicitValues(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"-stream-scheme=ws"}, "ws"},
		{[]string{"-stream-scheme=wss"}, "wss"},
	}
	for _, tt := range tests {
		got, err := ParseStreamScheme(tt.args)
		if err != nil {
			t.Fatalf("ParseStreamScheme(%v): unexpected error: %v", tt.args, err)
		}
		if got != tt.want {
			t.Fatalf("ParseStreamScheme(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestParseStreamScheme_RejectsInvalidValues(t *testing.T) {
	tests := []string{"http", "https", "WSS", "", "ws2"}
	for _, v := range tests {
		_, err := ParseStreamScheme([]string{"-stream-scheme=" + v})
		if err == nil {
			t.Fatalf("ParseStreamScheme(-stream-scheme=%q): expected error, got nil", v)
		}
	}
}
