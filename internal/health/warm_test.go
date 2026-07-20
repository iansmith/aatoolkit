package health_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/health"
)

// The warm-up must send exactly what the config string said: method, path,
// and the body verbatim. aa-server-status does not parse the body, so anything
// that mangles it silently breaks whichever server needed the warm-up.
func TestWarm_SendsConfiguredRequestVerbatim(t *testing.T) {
	var (
		mu     sync.Mutex
		gotM   string
		gotP   string
		gotB   string
		called int
	)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotM, gotP, gotB, called = r.Method, r.URL.Path, string(b), called+1
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	var wc config.Warm
	if err := wc.UnmarshalTOML("POST /v1/chat/completions " + body); err != nil {
		t.Fatalf("config: %v", err)
	}

	_, err := health.Warm(context.Background(), health.WarmConfig{
		Spec:         health.ResolveWarmSpec(wc, host, port),
		PollInterval: time.Millisecond,
		Timeout:      5 * time.Second,
		ServerName:   "warm-verbatim",
	})
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Errorf("warm-up sent %d times, want 1", called)
	}
	if gotM != "POST" {
		t.Errorf("method = %q, want POST", gotM)
	}
	if gotP != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotP)
	}
	if gotB != body {
		t.Errorf("body was not sent verbatim.\n got: %s\nwant: %s", gotB, body)
	}
}

// Only the status code matters: a 2xx with a body aa-server-status can't parse
// (or an empty one) is still a successful warm-up.
func TestWarm_OnlyStatusMatters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json at all <<<>>>"))
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	var wc config.Warm
	_ = wc.UnmarshalTOML("POST /warm {}")

	if _, err := health.Warm(context.Background(), health.WarmConfig{
		Spec:         health.ResolveWarmSpec(wc, host, port),
		PollInterval: time.Millisecond,
		Timeout:      5 * time.Second,
		ServerName:   "warm-status-only",
	}); err != nil {
		t.Errorf("a 2xx with an unparseable body must still count as warm: %v", err)
	}
}

// A non-2xx keeps the warm-up retrying rather than declaring the server ready.
func TestWarm_NonSuccessRetriesThenTimesOut(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	var wc config.Warm
	_ = wc.UnmarshalTOML("POST /warm {}")

	_, err := health.Warm(context.Background(), health.WarmConfig{
		Spec:         health.ResolveWarmSpec(wc, host, port),
		PollInterval: time.Millisecond,
		Timeout:      50 * time.Millisecond,
		ServerName:   "warm-fails",
		LogPath:      "build/logs/warm-fails.log",
	})
	if err == nil {
		t.Fatal("a warm-up that never returns 2xx must fail, not report ready")
	}
	for _, want := range []string{"warm-fails", "warm-up", "/warm", "build/logs/warm-fails.log"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so the failure is actionable, got: %v", want, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Errorf("warm-up attempted %d times, want retries before giving up", calls)
	}
}

// A slow warm-up is the normal case (a cold 30GB weight-load), not a failure:
// it must be awaited rather than cut off and retried. This is what makes the
// warm-up different from a health probe, which is deliberately bounded.
//
// The server is held open on a channel rather than a sleep, so the assertion
// is on what happened, not on how long it took: Warm must not return until the
// response does, and the server must be hit exactly once. A per-attempt
// timeout would show up as a second request, whatever its duration.
func TestWarm_SlowResponseIsAwaitedNotAbandoned(t *testing.T) {
	var (
		mu      sync.Mutex
		entered = make(chan struct{}, 8)
		hits    int
	)
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		entered <- struct{}{}
		<-release // the model loading: however long that takes
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	var wc config.Warm
	_ = wc.UnmarshalTOML("POST /warm {}")

	done := make(chan error, 1)
	go func() {
		_, err := health.Warm(context.Background(), health.WarmConfig{
			Spec:         health.ResolveWarmSpec(wc, host, port),
			PollInterval: time.Millisecond,
			Timeout:      30 * time.Second, // a backstop, not a deadline under test
			ServerName:   "warm-slow",
		})
		done <- err
	}()

	<-entered // Warm has reached the server, which is now "loading"

	select {
	case err := <-done:
		t.Fatalf("Warm returned while the server was still working (err=%v): a warm-up must await the load it provoked", err)
	case <-time.After(50 * time.Millisecond):
		// Nothing to assert on this interval -- it only gives an incorrect
		// implementation room to return early. The real assertion is below.
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Warm failed once the server answered: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("server was hit %d times, want 1: a slow warm-up was abandoned and retried rather than awaited", hits)
	}
}

func splitHostPort(t *testing.T, url string) (string, int) {
	t.Helper()
	hp := strings.TrimPrefix(url, "http://")
	host, portStr, ok := strings.Cut(hp, ":")
	if !ok {
		t.Fatalf("bad test server URL %q", url)
	}
	port := 0
	for _, r := range portStr {
		port = port*10 + int(r-'0')
	}
	return host, port
}

// A 4xx means the server understood the warm-up and refused it: the config is
// wrong, and no amount of asking again will change that. Retrying it for the
// whole ready-timeout would make an operator wait minutes for a diagnosis the
// first response already delivered.
func TestWarm_RejectedRequestFailsFastInsteadOfRetrying(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest) // e.g. a model name mlx-serve does not have
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	var wc config.Warm
	_ = wc.UnmarshalTOML("POST /warm {}")

	_, err := health.Warm(context.Background(), health.WarmConfig{
		Spec:         health.ResolveWarmSpec(wc, host, port),
		PollInterval: time.Millisecond,
		Timeout:      30 * time.Second, // a backstop: reaching it IS the bug
		ServerName:   "warm-rejected",
		LogPath:      "build/logs/warm-rejected.log",
	})
	if err == nil {
		t.Fatal("a rejected warm-up must fail, not report ready")
	}
	for _, want := range []string{"400", "rejected", "warm", "build/logs/warm-rejected.log"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so the config error is actionable, got: %v", want, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("server was asked %d times, want 1: a request the server rejects cannot be fixed by repeating it", calls)
	}
}

// 5xx is not a rejection: a server still loading may answer 500 or 503 before
// it can answer properly, which is exactly what the retry loop is for.
func TestWarm_ServerErrorIsStillRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // "loading"
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	var wc config.Warm
	_ = wc.UnmarshalTOML("POST /warm {}")

	if _, err := health.Warm(context.Background(), health.WarmConfig{
		Spec:         health.ResolveWarmSpec(wc, host, port),
		PollInterval: time.Millisecond,
		Timeout:      30 * time.Second,
		ServerName:   "warm-503",
		LogPath:      "build/logs/warm-503.log",
	}); err != nil {
		t.Fatalf("a server answering 503 while it loads must be retried, not given up on: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Errorf("server was asked %d times, want 3 (two 503s then a 200)", calls)
	}
}
