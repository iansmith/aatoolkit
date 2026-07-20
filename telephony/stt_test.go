package telephony

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sttStub is a fake whisper sidecar. It records what each request carried so
// tests can assert on the audio that actually reached the wire, and never calls
// t.Fatalf from the handler goroutine (FailNow is only valid on the test's own
// goroutine; from a handler it kills the response instead of the test).
type sttStub struct {
	mu              sync.Mutex
	requests        int
	audio           [][]byte // WAV payload of each request, in order
	responseFormats []string // response_format field of each request, in order
	requestIds      []string // request_id field of each request, in order
	reply           string
	echoRequestId   string // if set, echo this value as request_id in response (overrides received value)
	status          int
}

func (s *sttStub) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}

		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path = %s, want /v1/audio/transcriptions", r.URL.Path)
		}

		var wav []byte
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		} else if file, _, err := r.FormFile("file"); err != nil {
			t.Errorf("missing file part: %v", err)
		} else {
			defer file.Close()
			if wav, err = io.ReadAll(file); err != nil {
				t.Errorf("read file part: %v", err)
			}
		}

		respFormat := r.FormValue("response_format")
		requestId := r.FormValue("request_id")

		s.mu.Lock()
		s.requests++
		s.audio = append(s.audio, wav)
		s.responseFormats = append(s.responseFormats, respFormat)
		s.requestIds = append(s.requestIds, requestId)
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		responseBody := map[string]any{
			"text":     s.reply,
			"language": "en",
			"duration": 2.5,
			"segments": []map[string]any{
				{
					"text":           s.reply,
					"start":          0.0,
					"end":            2.5,
					"avg_logprob":    -0.2,
					"no_speech_prob": 0.01,
				},
			},
		}
		// Echo request_id if present or if an override is set
		echoValue := s.echoRequestId
		if echoValue == "" {
			echoValue = requestId
		}
		if echoValue != "" {
			responseBody["request_id"] = echoValue
		}
		_ = json.NewEncoder(w).Encode(responseBody)
	}
}

// audioBytes is the total PCM payload (WAV bytes minus headers) the stub saw.
func (s *sttStub) audioBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.audio {
		n += len(w) - wavHeaderSize
	}
	return n
}

func newSTTStub(t *testing.T, s *sttStub) *STTClient {
	t.Helper()
	server := httptest.NewServer(s.handler(t))
	t.Cleanup(server.Close)
	return NewSTTClient(server.URL)
}

func TestSTTClient_Transcribe(t *testing.T) {
	stub := &sttStub{reply: "hello world"}
	client := newSTTStub(t, stub)

	result, err := client.Transcribe(context.Background(), "", make([]byte, sttSampleRate))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if result.Text != "hello world" {
		t.Errorf("text = %q, want %q", result.Text, "hello world")
	}
	if stub.requests != 1 {
		t.Errorf("requests = %d, want 1", stub.requests)
	}
	if got := string(stub.audio[0][:4]); got != "RIFF" {
		t.Errorf("payload is not a WAV: magic = %q", got)
	}
	if stub.responseFormats[0] != "verbose_json" {
		t.Errorf("response_format = %q, want %q", stub.responseFormats[0], "verbose_json")
	}
}

// TestSTTClient_ChunkBoundary pins the 30s split point. maxClipBytes exactly is
// one request; one byte over is two. A <= / < flip here would silently change
// how every long utterance is transcribed, so it gets an explicit test.
func TestSTTClient_ChunkBoundary(t *testing.T) {
	for _, tt := range []struct {
		name         string
		clip         int
		wantRequests int
	}{
		{"one byte under 30s", maxClipBytes - 1, 1},
		{"exactly 30s", maxClipBytes, 1},
		{"one byte over 30s", maxClipBytes + 1, 2},
		{"exactly 60s", 2 * maxClipBytes, 2},
		{"one byte over 60s", 2*maxClipBytes + 1, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := &sttStub{reply: "x"}
			client := newSTTStub(t, stub)

			if _, err := client.Transcribe(context.Background(), "", make([]byte, tt.clip)); err != nil {
				t.Fatalf("Transcribe: %v", err)
			}

			if stub.requests != tt.wantRequests {
				t.Errorf("requests = %d, want %d", stub.requests, tt.wantRequests)
			}
			// No audio may be dropped or duplicated at a chunk boundary: one
			// μ-law byte in, one 16-bit PCM sample out.
			if got, want := stub.audioBytes(), tt.clip*2; got != want {
				t.Errorf("PCM bytes delivered = %d, want %d", got, want)
			}
		})
	}
}

func TestSTTClient_ChunkedTranscriptsJoinedInOrder(t *testing.T) {
	stub := &sttStub{reply: "chunk"}
	client := newSTTStub(t, stub)

	// 250000 bytes = 31.25s -> exactly 2 chunks.
	result, err := client.Transcribe(context.Background(), "", make([]byte, 250000))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if stub.requests != 2 {
		t.Fatalf("requests = %d, want 2", stub.requests)
	}
	// Literal expectation -- NOT derived from the observed request count, which
	// would pass for any chunking behavior at all.
	if result.Text != "chunk chunk" {
		t.Errorf("text = %q, want %q", result.Text, "chunk chunk")
	}
}

func TestSTTClient_EmptyClipMakesNoRequest(t *testing.T) {
	stub := &sttStub{reply: "should not be called"}
	client := newSTTStub(t, stub)

	result, err := client.Transcribe(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if result.Text != "" {
		t.Errorf("text = %q, want empty", result.Text)
	}
	if stub.requests != 0 {
		t.Errorf("requests = %d, want 0 -- an empty clip must not hit the sidecar", stub.requests)
	}
}

func TestSTTClient_ErrorStatus(t *testing.T) {
	stub := &sttStub{status: http.StatusInternalServerError}
	client := newSTTStub(t, stub)

	if _, err := client.Transcribe(context.Background(), "", make([]byte, sttSampleRate)); err == nil {
		t.Error("expected an error on HTTP 500, got nil")
	}
}

// TestSTTClient_ContextCancellation proves a wedged sidecar cannot pin the
// caller: a cancelled context aborts the in-flight request.
func TestSTTClient_ContextCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang until the test releases us
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	client := NewSTTClient(server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { _, err := client.Transcribe(ctx, "", make([]byte, sttSampleRate)); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error when the context is cancelled, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Transcribe ignored context cancellation and hung")
	}
}

// TestSTTResult_VerboseJSON pins the full whisper verbose_json decode: language,
// duration, and per-segment timing/confidence fields, not just the transcript text.
func TestSTTResult_VerboseJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text":     "Hello world",
			"language": "en",
			"duration": 2.5,
			"segments": []map[string]any{
				{
					"text":           "Hello world",
					"start":          0.0,
					"end":            2.5,
					"avg_logprob":    -0.25,
					"no_speech_prob": 0.02,
				},
			},
		})
	}))
	t.Cleanup(server.Close)

	client := NewSTTClient(server.URL)
	result, err := client.Transcribe(context.Background(), "", make([]byte, sttSampleRate))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if result.Text != "Hello world" {
		t.Errorf("Text = %q, want %q", result.Text, "Hello world")
	}
	if result.Language != "en" {
		t.Errorf("Language = %q, want %q", result.Language, "en")
	}
	if result.Duration < 2.4999 || result.Duration > 2.5001 {
		t.Errorf("Duration = %v, want ~2.5", result.Duration)
	}

	if len(result.Segments) < 1 {
		t.Fatalf("Segments = %v, want at least 1", result.Segments)
	}
	seg := result.Segments[0]
	if seg.Text != "Hello world" {
		t.Errorf("Segments[0].Text = %q, want %q", seg.Text, "Hello world")
	}
	if seg.Start < -0.0001 || seg.Start > 0.0001 {
		t.Errorf("Segments[0].Start = %v, want ~0.0", seg.Start)
	}
	if seg.End < 2.4999 || seg.End > 2.5001 {
		t.Errorf("Segments[0].End = %v, want ~2.5", seg.End)
	}
	if seg.AvgLogprob >= 0 {
		t.Errorf("Segments[0].AvgLogprob = %v, want a negative float", seg.AvgLogprob)
	}
	if seg.NoSpeechProb >= 1.0 {
		t.Errorf("Segments[0].NoSpeechProb = %v, want < 1.0", seg.NoSpeechProb)
	}
}

// TestSTTService_CorrelationPassthrough proves the sttService wrapper -- not
// Transcribe itself -- is what copies SessionID/RequestID/Kind from request to
// result. Transcribe only ever sees audio bytes.
func TestSTTService_CorrelationPassthrough(t *testing.T) {
	stub := &sttStub{reply: "hello world"}
	client := newSTTStub(t, stub)

	input := NewBufferedChan[STTRequest](1)
	output := NewBufferedChan[STTResult](1)
	svc := newSTTService(client, input, output)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.run(ctx)
	}()

	req := STTRequest{
		SessionID: "call-1",
		RequestID: 7,
		Kind:      FullPass,
		Audio:     make([]byte, sttSampleRate),
	}
	if err := input.Send(ctx, req); err != nil {
		t.Fatalf("Send request: %v", err)
	}

	result, err := output.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv result: %v", err)
	}
	cancel()
	<-done

	if result.SessionID != req.SessionID {
		t.Errorf("SessionID = %q, want %q", result.SessionID, req.SessionID)
	}
	if result.RequestID != req.RequestID {
		t.Errorf("RequestID = %d, want %d", result.RequestID, req.RequestID)
	}
	if result.Kind != req.Kind {
		t.Errorf("Kind = %v, want %v", result.Kind, req.Kind)
	}
	if result.Text != "hello world" {
		t.Errorf("Text = %q, want %q", result.Text, "hello world")
	}
}

// TestTranscribeEchoesId verifies that Transcribe sends the id parameter as
// request_id in the multipart form and decodes the echoed value without error.
func TestTranscribeEchoesId(t *testing.T) {
	stub := &sttStub{reply: "hello world"}
	client := newSTTStub(t, stub)

	result, err := client.Transcribe(context.Background(), "42", make([]byte, sttSampleRate))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if stub.requests != 1 {
		t.Errorf("requests = %d, want 1", stub.requests)
	}
	if len(stub.requestIds) < 1 {
		t.Fatalf("stub.requestIds is empty, want at least 1")
	}
	if stub.requestIds[0] != "42" {
		t.Errorf("stub received request_id = %q, want %q", stub.requestIds[0], "42")
	}
	if result.Text != "hello world" {
		t.Errorf("Text = %q, want %q", result.Text, "hello world")
	}
}

// TestTranscribeIdMismatch verifies that Transcribe returns an error when the
// echoed request_id does not match the sent id.
func TestTranscribeIdMismatch(t *testing.T) {
	stub := &sttStub{reply: "hello world", echoRequestId: "99"}
	client := newSTTStub(t, stub)

	_, err := client.Transcribe(context.Background(), "42", make([]byte, sttSampleRate))
	if err == nil {
		t.Errorf("expected an error on id mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "id mismatch") {
		t.Errorf("error message = %q, want substring %q", err.Error(), "id mismatch")
	}
}

// TestTranscribeNoId verifies backward compatibility when no id is provided.
func TestTranscribeNoId(t *testing.T) {
	stub := &sttStub{reply: "hello world"}
	client := newSTTStub(t, stub)

	result, err := client.Transcribe(context.Background(), "", make([]byte, sttSampleRate))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if stub.requests != 1 {
		t.Errorf("requests = %d, want 1", stub.requests)
	}
	if len(stub.requestIds) < 1 {
		t.Fatalf("stub.requestIds is empty, want at least 1")
	}
	if stub.requestIds[0] != "" {
		t.Errorf("stub received request_id = %q, want empty string", stub.requestIds[0])
	}
	if result.Text != "hello world" {
		t.Errorf("Text = %q, want %q", result.Text, "hello world")
	}
}

// TestSTTServiceCancelSkipsPost verifies that a request in the abandoned set is
// never POSTed to the sidecar.
func TestSTTServiceCancelSkipsPost(t *testing.T) {
	blockFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	stub := &sttStub{
		reply:  "hello",
		status: 0,
	}

	// Override the handler to block on the first request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path = %s, want /v1/audio/transcriptions", r.URL.Path)
		}

		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		} else if file, _, err := r.FormFile("file"); err != nil {
			t.Errorf("missing file part: %v", err)
		} else {
			defer file.Close()
			_, _ = io.ReadAll(file)
		}

		requestId := r.FormValue("request_id")

		stub.mu.Lock()
		requestNum := stub.requests
		stub.requests++
		stub.mu.Unlock()

		// Block on first request
		if requestNum == 0 {
			close(blockFirst)
			<-releaseFirst
		}

		w.Header().Set("Content-Type", "application/json")
		responseBody := map[string]any{
			"text":     stub.reply,
			"language": "en",
			"duration": 1.0,
			"segments": []map[string]any{},
		}
		if requestId != "" {
			responseBody["request_id"] = requestId
		}
		_ = json.NewEncoder(w).Encode(responseBody)
	}))
	t.Cleanup(server.Close)

	client := NewSTTClient(server.URL)
	input := NewBufferedChan[STTRequest](2)
	output := NewBufferedChan[STTResult](10)
	svc := newSTTService(client, input, output)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.run(ctx)
	}()

	// Send two requests
	req1 := STTRequest{
		SessionID: "call-1",
		RequestID: 1,
		Kind:      FullPass,
		Audio:     make([]byte, sttSampleRate),
	}
	req2 := STTRequest{
		SessionID: "call-1",
		RequestID: 2,
		Kind:      FullPass,
		Audio:     make([]byte, sttSampleRate),
	}

	if err := input.Send(ctx, req1); err != nil {
		t.Fatalf("Send req1: %v", err)
	}
	if err := input.Send(ctx, req2); err != nil {
		t.Fatalf("Send req2: %v", err)
	}

	// Wait for the first request to start
	<-blockFirst

	// Send a cancel for the second request before the first completes
	select {
	case svc.CancelChan <- sttAbandon{SessionID: "call-1", RequestID: 2}:
	case <-time.After(1 * time.Second):
		t.Fatal("timeout sending cancel")
	}

	// Release the first request and allow it to complete
	close(releaseFirst)

	// Consume the result from the first request with a timeout
	resultChan := make(chan STTResult, 1)
	errChan := make(chan error, 1)
	go func() {
		result, err := output.Recv(ctx)
		if err != nil {
			errChan <- err
		} else {
			resultChan <- result
		}
	}()

	var result STTResult
	select {
	case result = <-resultChan:
	case err := <-errChan:
		t.Fatalf("Recv result: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for result1")
	}

	// Give the service a moment to process the cancelled request
	time.Sleep(200 * time.Millisecond)

	// Verify the results
	if stub.requests != 1 {
		t.Errorf("requests = %d, want 1 (the cancelled request should not have been POSTed)", stub.requests)
	}
	if result.RequestID != 1 {
		t.Errorf("result.RequestID = %d, want 1", result.RequestID)
	}
}

// TestSTTServiceAbandonedSetIsBounded verifies that the abandoned-set built
// from cancels does not grow without bound: a cancel for a request that is
// never dequeued (the common case, since the GPU runs a request to
// completion regardless of cancellation) must eventually be evicted rather
// than pinned in the map forever. It sends far more distinct cancels than
// maxAbandoned, forcing drains between batches via a same-session request
// that must round-trip, then checks that the oldest cancelled RequestID has
// aged out (its request now reaches the stub) while a recent one is still
// honored.
func TestSTTServiceAbandonedSetIsBounded(t *testing.T) {
	stub := &sttStub{reply: "hello"}
	server := httptest.NewServer(stub.handler(t))
	t.Cleanup(server.Close)

	client := NewSTTClient(server.URL)
	input := NewBufferedChan[STTRequest](1)
	output := NewBufferedChan[STTResult](1)
	svc := newSTTService(client, input, output)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	const session = "cancel-flood"
	const total = maxAbandoned + 100

	// drainID space is disjoint from the flooded cancel IDs so each round
	// trip forces a drain of that batch's cancels without ever matching one.
	for i := 1; i <= total; i++ {
		select {
		case svc.CancelChan <- sttAbandon{SessionID: session, RequestID: i}:
		case <-time.After(1 * time.Second):
			t.Fatalf("timeout sending cancel %d", i)
		}

		if i%100 == 0 || i == total {
			drainReq := STTRequest{
				SessionID: session,
				RequestID: -i, // disjoint from any flooded/cancelled RequestID
				Kind:      FullPass,
				Audio:     make([]byte, sttSampleRate),
			}
			if err := input.Send(ctx, drainReq); err != nil {
				t.Fatalf("Send drain request %d: %v", i, err)
			}
			if _, err := output.Recv(ctx); err != nil {
				t.Fatalf("Recv drain result %d: %v", i, err)
			}
		}
	}

	stub.mu.Lock()
	postsAfterFlood := stub.requests
	stub.mu.Unlock()

	// RequestID 1 was the first cancelled and should have aged out of the
	// bounded set; its request must now actually reach the stub.
	oldReq := STTRequest{SessionID: session, RequestID: 1, Kind: FullPass, Audio: make([]byte, sttSampleRate)}
	if err := input.Send(ctx, oldReq); err != nil {
		t.Fatalf("Send oldReq: %v", err)
	}
	if _, err := output.Recv(ctx); err != nil {
		t.Fatalf("Recv oldReq result: %v", err)
	}

	stub.mu.Lock()
	postsAfterOld := stub.requests
	stub.mu.Unlock()
	if postsAfterOld != postsAfterFlood+1 {
		t.Errorf("posts after re-sending evicted RequestID 1 = %d, want %d (request should have reached the stub, not been skipped)", postsAfterOld, postsAfterFlood+1)
	}

	// RequestID `total` was cancelled most recently and must still be
	// honored: sending it should NOT produce an output result.
	recentReq := STTRequest{SessionID: session, RequestID: total, Kind: FullPass, Audio: make([]byte, sttSampleRate)}
	if err := input.Send(ctx, recentReq); err != nil {
		t.Fatalf("Send recentReq: %v", err)
	}

	// Push one more real request through so we have a deterministic signal
	// that the service moved past recentReq without emitting a result for it.
	sentinel := STTRequest{SessionID: session, RequestID: total + 1, Kind: FullPass, Audio: make([]byte, sttSampleRate)}
	if err := input.Send(ctx, sentinel); err != nil {
		t.Fatalf("Send sentinel: %v", err)
	}
	result, err := output.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv sentinel result: %v", err)
	}
	if result.RequestID != total+1 {
		t.Errorf("result.RequestID = %d, want %d (recentReq should have been skipped, not posted)", result.RequestID, total+1)
	}

	stub.mu.Lock()
	postsAfterRecent := stub.requests
	stub.mu.Unlock()
	if postsAfterRecent != postsAfterOld+1 {
		t.Errorf("posts after recentReq+sentinel = %d, want %d (recentReq must not have reached the stub)", postsAfterRecent, postsAfterOld+1)
	}
}
