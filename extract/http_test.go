package extract

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtractClient_PostsFixtureRequestAndDecodesSpans(t *testing.T) {
	// Load fixture for validation
	fixture := loadFixture(t)
	expectedRequest := fixture["request_json"].(string)
	expectedSpans := fixture["response"].(map[string]interface{})["spans"].([]interface{})

	var requestCount int
	var recordedBody string
	var recordedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/extract" {
			t.Errorf("expected path /extract, got %s", r.URL.Path)
		}

		recordedContentType = r.Header.Get("Content-Type")

		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		recordedBody = string(buf[:n])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(fixture["response_json"].(string)))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	req := Request{
		Text:      "Bill on the 5th of July",
		Labels:    []string{"birthday_person", "birthday_date"},
		Threshold: 0.5,
	}

	spans, err := client.Extract(context.Background(), req)
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	if recordedContentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", recordedContentType, "application/json")
	}

	if recordedBody != expectedRequest {
		t.Errorf("request body mismatch\nGot:      %s\nExpected: %s", recordedBody, expectedRequest)
	}

	if len(spans) != len(expectedSpans) {
		t.Errorf("expected %d spans, got %d", len(expectedSpans), len(spans))
	}

	if len(spans) >= 2 {
		if spans[0].Text != "Bill" || spans[0].Label != "birthday_person" || spans[0].Start != 0 || spans[0].End != 4 || spans[0].Score != 0.87 {
			t.Errorf("span[0] mismatch: %+v", spans[0])
		}
		if spans[1].Text != "5th of July" || spans[1].Label != "birthday_date" || spans[1].Start != 12 || spans[1].End != 23 || spans[1].Score != 0.81 {
			t.Errorf("span[1] mismatch: %+v", spans[1])
		}
	}
}

func TestExtractClient_ImplementsExtractor(t *testing.T) {
	var _ Extractor = (*Client)(nil)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture := loadFixture(t)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fixture["response_json"].(string)))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	var extractor Extractor = client

	spans, err := extractor.Extract(context.Background(), Request{
		Text:      "Bill on the 5th of July",
		Labels:    []string{"birthday_person", "birthday_date"},
		Threshold: 0.5,
	})
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}
}

func TestExtractClient_NoRetryOnServerError(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"sidecar error"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	_, err := client.Extract(context.Background(), Request{
		Text:      "test",
		Labels:    []string{"label"},
		Threshold: 0.5,
	})

	if err == nil {
		t.Fatal("expected error for 500 response")
	}

	if !strings.Contains(err.Error(), "extract sidecar returned 500") {
		t.Errorf("error message should contain 'extract sidecar returned 500', got: %v", err)
	}

	if requestCount != 1 {
		t.Errorf("expected exactly 1 request, got %d", requestCount)
	}
}

func TestExtractClient_TransportErrorNamesSidecar(t *testing.T) {
	client := NewClient("http://127.0.0.1:1")

	_, err := client.Extract(context.Background(), Request{
		Text:      "test",
		Labels:    []string{"label"},
		Threshold: 0.5,
	})

	if err == nil {
		t.Fatal("expected error for unreachable sidecar")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "calling extract sidecar") {
		t.Errorf("error should contain 'calling extract sidecar', got: %v", err)
	}
	if !strings.Contains(errMsg, "127.0.0.1:1") {
		t.Errorf("error should contain URL, got: %v", err)
	}
}

func TestExtractClient_GuardsRejectWithoutHTTPCall(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"spans":[]}`))
	}))
	defer server.Close()

	tests := []struct {
		name    string
		req     Request
		wantErr bool
		wantNil bool
	}{
		{
			name:    "empty text",
			req:     Request{Text: "", Labels: []string{"label"}, Threshold: 0.5},
			wantErr: false,
			wantNil: true,
		},
		{
			name:    "nil labels",
			req:     Request{Text: "test", Labels: nil, Threshold: 0.5},
			wantErr: true,
			wantNil: false,
		},
		{
			name:    "empty labels slice",
			req:     Request{Text: "test", Labels: []string{}, Threshold: 0.5},
			wantErr: true,
			wantNil: false,
		},
		{
			name:    "threshold too low",
			req:     Request{Text: "test", Labels: []string{"label"}, Threshold: -0.1},
			wantErr: true,
			wantNil: false,
		},
		{
			name:    "threshold too high",
			req:     Request{Text: "test", Labels: []string{"label"}, Threshold: 1.5},
			wantErr: true,
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestCount = 0
			client := NewClient(server.URL)
			spans, err := client.Extract(context.Background(), tt.req)

			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if tt.wantNil && spans != nil {
				t.Errorf("expected nil spans, got %v", spans)
			}
			if !tt.wantNil && spans == nil && !tt.wantErr {
				t.Error("expected non-nil spans")
			}

			if requestCount != 0 {
				t.Errorf("expected 0 HTTP requests for guard, got %d", requestCount)
			}
		})
	}
}

func TestExtractClient_AllowsSlowSidecar(t *testing.T) {
	fixture := loadFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fixture["response_json"].(string)))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	spans, err := client.Extract(ctx, Request{
		Text:      "Bill on the 5th of July",
		Labels:    []string{"birthday_person", "birthday_date"},
		Threshold: 0.5,
	})

	if err != nil {
		t.Fatalf("Extract with slow sidecar failed: %v", err)
	}
	if len(spans) != 2 {
		t.Errorf("expected 2 spans, got %d", len(spans))
	}
}

func TestExtractClient_RespectsContextCancellation(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		time.Sleep(500 * time.Millisecond)
		fixture := loadFixture(t)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fixture["response_json"].(string)))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err := client.Extract(ctx, Request{
		Text:      "Bill on the 5th of July",
		Labels:    []string{"birthday_person", "birthday_date"},
		Threshold: 0.5,
	})

	if err == nil {
		t.Fatal("expected error for cancelled context")
	}

	if requestCount > 1 {
		t.Errorf("expected at most 1 request, got %d", requestCount)
	}
}

func TestExtractClient_TimeoutIsGenerous(t *testing.T) {
	if defaultExtractTimeout < 5*time.Second {
		t.Errorf("defaultExtractTimeout = %v, want >= 5s", defaultExtractTimeout)
	}
	if defaultExtractTimeout != 30*time.Second {
		t.Errorf("defaultExtractTimeout = %v, want 30s", defaultExtractTimeout)
	}

	client := NewClient("http://example.com")
	if client.httpClient.Timeout != defaultExtractTimeout {
		t.Errorf("http.Client.Timeout = %v, want %v", client.httpClient.Timeout, defaultExtractTimeout)
	}
}
