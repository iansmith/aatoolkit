package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
)

// --- ResolveSpec: edge/boundary cases ---

func TestResolveSpec_DefaultsHostAndPortFromServer(t *testing.T) {
	spec := ResolveSpec(config.Health{Path: "/v1/models"}, "127.0.0.1", 9000)
	if spec.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want server host default", spec.Host)
	}
	if spec.Port != 9000 {
		t.Errorf("Port = %d, want server port default", spec.Port)
	}
	if spec.Path != "/v1/models" {
		t.Errorf("Path = %q, want /v1/models", spec.Path)
	}
}

func TestResolveSpec_ExplicitHostAndPortOverrideServerDefaults(t *testing.T) {
	spec := ResolveSpec(config.Health{Host: "10.0.0.5", Port: 9100, Path: "/healthz"}, "127.0.0.1", 9000)
	if spec.Host != "10.0.0.5" {
		t.Errorf("Host = %q, want explicit override 10.0.0.5", spec.Host)
	}
	if spec.Port != 9100 {
		t.Errorf("Port = %d, want explicit override 9100", spec.Port)
	}
}

func TestResolveSpec_PartialOverridePreservesOtherDefault(t *testing.T) {
	// Only Port set explicitly — Host should still default to server host.
	spec := ResolveSpec(config.Health{Port: 9200, Path: "/healthz"}, "192.168.1.1", 9000)
	if spec.Host != "192.168.1.1" {
		t.Errorf("Host = %q, want default 192.168.1.1 preserved", spec.Host)
	}
	if spec.Port != 9200 {
		t.Errorf("Port = %d, want explicit override 9200", spec.Port)
	}
}

// --- Probe: error/rejection cases first ---

func TestProbe_ConnectErrorIsNotHealthy(t *testing.T) {
	// Nothing listens here — connection should fail/refuse.
	spec := Spec{Host: "127.0.0.1", Port: 1, Path: "/healthz"}
	result := Probe(context.Background(), spec, 200*time.Millisecond)
	if result.Healthy {
		t.Error("Healthy = true, want false on connect error")
	}
	if result.Err == nil {
		t.Error("Err = nil, want non-nil connect error")
	}
}

func TestProbe_TimeoutIsNotHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/slow")
	result := Probe(context.Background(), spec, 50*time.Millisecond)
	if result.Healthy {
		t.Error("Healthy = true, want false on timeout")
	}
	if result.Err == nil {
		t.Error("Err = nil, want timeout error")
	}
}

func TestProbe_5xxIsNotHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/healthz")
	result := Probe(context.Background(), spec, time.Second)
	if result.Healthy {
		t.Error("Healthy = true, want false for 500 response")
	}
	if result.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", result.StatusCode)
	}
}

func TestProbe_4xxIsNotHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/healthz")
	result := Probe(context.Background(), spec, time.Second)
	if result.Healthy {
		t.Error("Healthy = true, want false for 404 response")
	}
}

func TestProbe_3xxIsNotHealthy(t *testing.T) {
	// Redirects are not 2xx and must not be silently followed into a pass.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/healthz")
	result := Probe(context.Background(), spec, time.Second)
	if result.Healthy {
		t.Error("Healthy = true, want false for 301 response")
	}
}

// --- Probe: happy path + rendered form ---

func TestProbe_2xxIsHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/v1/models")
	result := Probe(context.Background(), spec, time.Second)
	if !result.Healthy {
		t.Fatalf("Healthy = false, want true; err=%v", result.Err)
	}
	if result.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", result.StatusCode)
	}
}

func TestProbe_RenderedFormForStatusTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/v1/models")
	result := Probe(context.Background(), spec, time.Second)
	want := "/v1/models 200"
	if result.Rendered != want {
		t.Errorf("Rendered = %q, want %q", result.Rendered, want)
	}
}

func TestProbe_RenderedFormOnConnectError(t *testing.T) {
	spec := Spec{Host: "127.0.0.1", Port: 1, Path: "/healthz"}
	result := Probe(context.Background(), spec, 200*time.Millisecond)
	if !strings.HasPrefix(result.Rendered, "/healthz") {
		t.Errorf("Rendered = %q, want prefix /healthz", result.Rendered)
	}
	if strings.Contains(result.Rendered, "200") {
		t.Errorf("Rendered = %q, must not claim 200 on connect error", result.Rendered)
	}
}

// --- PollReady: cross-feature interaction / loop behavior ---

func TestPollReady_SucceedsImmediatelyWithoutPolling(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/v1/models")
	result, err := PollReady(context.Background(), PollConfig{
		Spec:         spec,
		ProbeTimeout: 200 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		ReadyTimeout: 2 * time.Second,
		ServerName:   "immediate-server",
		LogPath:      "/tmp/immediate-server.log",
	})
	if err != nil {
		t.Fatalf("PollReady returned error: %v", err)
	}
	if !result.Healthy {
		t.Error("Healthy = false, want true on first probe")
	}
	if probes.Load() != 1 {
		t.Errorf("probes = %d, want exactly 1 (no unnecessary retries)", probes.Load())
	}
}

func TestPollReady_AttemptsAtLeastOneProbeEvenUnderTightReadyTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/v1/models")
	// ReadyTimeout shorter than PollInterval must still attempt one probe
	// rather than timing out with zero attempts made.
	result, err := PollReady(context.Background(), PollConfig{
		Spec:         spec,
		ProbeTimeout: 200 * time.Millisecond,
		PollInterval: 500 * time.Millisecond,
		ReadyTimeout: 1 * time.Millisecond,
		ServerName:   "tight-timeout-server",
		LogPath:      "/tmp/tight-timeout-server.log",
	})
	if err != nil {
		t.Fatalf("PollReady returned error: %v, want at least one probe to succeed", err)
	}
	if !result.Healthy {
		t.Error("Healthy = false, want true — first probe should have been attempted")
	}
}

func TestPollReady_SucceedsOnceServerStartsResponding(t *testing.T) {
	var readyAfter atomic.Int32
	readyAfter.Store(3) // fail the first 3 probes, then succeed

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if readyAfter.Add(-1) >= 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/v1/models")
	result, err := PollReady(context.Background(), PollConfig{
		Spec:         spec,
		ProbeTimeout: 200 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		ReadyTimeout: 2 * time.Second,
		ServerName:   "test-server",
		LogPath:      "/tmp/test-server.log",
	})
	if err != nil {
		t.Fatalf("PollReady returned error: %v", err)
	}
	if !result.Healthy {
		t.Error("Healthy = false, want true after retries succeed")
	}
}

func TestPollReady_TimesOutLoudlyWithLogPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	spec := specFromURL(t, srv.URL, "/v1/models")
	_, err := PollReady(context.Background(), PollConfig{
		Spec:         spec,
		ProbeTimeout: 50 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		ReadyTimeout: 100 * time.Millisecond,
		ServerName:   "cold-mlx",
		LogPath:      "/var/log/server/cold-mlx.log",
	})
	if err == nil {
		t.Fatal("PollReady error = nil, want timeout error")
	}
	if !strings.Contains(err.Error(), "cold-mlx") {
		t.Errorf("error %q must name the server", err.Error())
	}
	if !strings.Contains(err.Error(), "/var/log/server/cold-mlx.log") {
		t.Errorf("error %q must carry the log path", err.Error())
	}
}

func TestPollReady_RespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	spec := specFromURL(t, srv.URL, "/v1/models")

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := PollReady(ctx, PollConfig{
		Spec:         spec,
		ProbeTimeout: 50 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		ReadyTimeout: 10 * time.Second, // long enough that only cancellation stops it
		ServerName:   "cancel-test",
		LogPath:      "/tmp/cancel-test.log",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("PollReady error = nil, want cancellation error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("PollReady took %v after cancellation, want prompt return", elapsed)
	}
}

// --- helpers ---

func specFromURL(t *testing.T, rawURL, path string) Spec {
	t.Helper()
	// httptest.Server.URL is like "http://127.0.0.1:54321"
	host, port := splitHostPort(t, rawURL)
	return Spec{Host: host, Port: port, Path: path}
}

func splitHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u := strings.TrimPrefix(rawURL, "http://")
	host, portStr, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatalf("could not split host:port from %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("could not parse port from %q: %v", rawURL, err)
	}
	return host, port
}
