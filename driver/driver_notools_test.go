package driver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// bodyRecordingServer answers one SSE delta and keeps the decoded JSON body of
// the last request.
func bodyRecordingServer(t *testing.T) (*httptest.Server, func() map[string]json.RawMessage) {
	t.Helper()
	var mu sync.Mutex
	var last map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		mu.Lock()
		last = body
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n")
	}))
	return srv, func() map[string]json.RawMessage {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// TestTierNoTools_DeclaresAnEmptyToolsList (AATK-113): a tier with NoTools
// sends "tools": [] and nothing else changes; a tier without it sends no tools
// key at all, byte-identical to the request before the field existed.
func TestTierNoTools_DeclaresAnEmptyToolsList(t *testing.T) {
	srv, last := bodyRecordingServer(t)
	defer srv.Close()

	h := New(Config{Tiers: map[string]Tier{"fast": {URL: srv.URL, Model: "test", MaxTokens: 8, NoTools: true}}, Prompt: func() string { return "" }})
	if _, _, err := h.Send([]byte("[]"), "fast"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got, ok := last()["tools"]; !ok || string(got) != "[]" {
		t.Errorf("tools in body = %q (present %v), want an empty array", got, ok)
	}

	h = New(Config{Tiers: map[string]Tier{"fast": {URL: srv.URL, Model: "test", MaxTokens: 8}}, Prompt: func() string { return "" }})
	if _, _, err := h.Send([]byte("[]"), "fast"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, ok := last()["tools"]; ok {
		t.Errorf("tools key present with NoTools unset; the default request must not change")
	}
}

// TestEmptyStream_ErrorNamesTheFinishReason (AATK-113): a stream that carries
// only a tool_calls delta and ends with finish_reason "tool_calls" is still an
// error -- this driver has no tools -- but the error says WHY there was no
// content, so the next operator does not need a raw probe to find out.
func TestEmptyStream_ErrorNamesTheFinishReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n"+
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"get_calendar_events\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n"+
			"data: [DONE]\n")
	}))
	defer srv.Close()

	h := New(Config{Tiers: map[string]Tier{"fast": {URL: srv.URL, Model: "test", MaxTokens: 8}}, Prompt: func() string { return "" }})
	_, _, err := h.Send([]byte("[]"), "fast")
	if err == nil {
		t.Fatal("Send returned no error for a stream with no content")
	}
	if !strings.Contains(err.Error(), "tool_calls") {
		t.Errorf("error = %v; want it to name finish_reason tool_calls", err)
	}
}
