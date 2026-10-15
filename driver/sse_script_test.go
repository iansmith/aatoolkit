package driver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// sseStep is one scripted line of an SSE response: wait delay, then write line
// plus a newline and flush it to the client.
type sseStep struct {
	delay time.Duration
	line  string
}

// sseRequestLog records the requests a scripted SSE server received. It is
// safe for concurrent use: the handler writes it while the test reads it.
type sseRequestLog struct {
	mu     sync.Mutex
	count  int
	body   []byte
	header http.Header
}

func (l *sseRequestLog) record(header http.Header, body []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
	l.body = body
	l.header = header
}

// LastBody returns the last request's body decoded as a JSON object, or nil
// when there was no request or the body is not a JSON object.
func (l *sseRequestLog) LastBody() map[string]json.RawMessage {
	raw := l.LastRawBody()
	if raw == nil {
		return nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	return body
}

// LastRawBody returns a copy of the last request's body bytes, or nil when
// there was no request.
func (l *sseRequestLog) LastRawBody() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count == 0 {
		return nil
	}
	return bytes.Clone(l.body)
}

// LastHeader returns a copy of the last request's headers, or nil when there
// was no request.
func (l *sseRequestLog) LastHeader() http.Header {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.header.Clone()
}

// Count returns how many requests the server has received.
func (l *sseRequestLog) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// newScriptedSSEServer starts an upstream that answers every request by
// replaying steps in order: for each, it waits the step's delay (giving up if
// the client goes away), writes the line and a newline, and flushes, so the
// client sees each line arrive at its scripted time. It reads and records each
// request first. The caller closes the server.
func newScriptedSSEServer(steps ...sseStep) (*httptest.Server, *sseRequestLog) {
	reqs := &sseRequestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqs.record(r.Header.Clone(), body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, step := range steps {
			if step.delay > 0 {
				timer := time.NewTimer(step.delay)
				select {
				case <-timer.C:
				case <-r.Context().Done():
					timer.Stop()
					return
				}
			}
			if _, err := io.WriteString(w, step.line+"\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	return srv, reqs
}
