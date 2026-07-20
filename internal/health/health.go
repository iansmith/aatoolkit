// Package health implements the mandatory health gate for aa-server-status
// servers: a single GET request whose 2xx response is the sole authoritative
// "serving" signal (design/aa-server-status.md §6.1 stage 3, §7.1). There is no
// any_http fallback — connect errors, timeouts, and non-2xx responses are
// all simply "not healthy."
package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iansmith/aatoolkit/config"
)

// Spec is a fully resolved health-check target: a host, port, and path to
// GET. Unlike config.Health, every field is guaranteed populated.
type Spec struct {
	Host string
	Port int
	Path string
}

// ResolveSpec fills in a per-server health spec from config.Health,
// defaulting Host and Port to the server's own values when left unset in
// the TOML. Path has no default — config.Validate already enforces it's
// non-empty before a Health value reaches here.
func ResolveSpec(h config.Health, serverHost string, serverPort int) Spec {
	spec := Spec{Host: h.Host, Port: h.Port, Path: h.Path}
	if spec.Host == "" {
		spec.Host = serverHost
	}
	if spec.Port == 0 {
		spec.Port = serverPort
	}
	return spec
}

// URL renders the spec as the GET target.
func (s Spec) URL() string {
	return fmt.Sprintf("http://%s:%d%s", s.Host, s.Port, s.Path)
}

// Result is the outcome of a single health probe.
type Result struct {
	// Healthy is true only when the GET completed with a 2xx status.
	Healthy bool
	// StatusCode is the response status, or 0 if no response was received
	// (connect error, timeout, or other request failure).
	StatusCode int
	// Err is the request-level error on failure to get any response.
	Err error
	// Rendered is the status-table form, e.g. "/v1/models 200" on a
	// completed request, or just the path if no status was received.
	Rendered string
}

// Probe issues a single GET against spec, bounded by timeout. A 2xx
// response is the only "healthy" outcome — anything else (non-2xx status,
// connect error, or timeout) is not healthy. This is the mandatory health
// gate: there is no fallback to a bare TCP connect check or other lesser
// signal.
func Probe(ctx context.Context, spec Spec, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.URL(), nil)
	if err != nil {
		return Result{Err: err, Rendered: spec.Path}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{Err: err, Rendered: spec.Path}
	}
	defer resp.Body.Close()

	return Result{
		Healthy:    resp.StatusCode >= 200 && resp.StatusCode < 300,
		StatusCode: resp.StatusCode,
		Rendered:   fmt.Sprintf("%s %d", spec.Path, resp.StatusCode),
	}
}

// WarmSpec is a fully resolved warm-up target: where to send the request,
// and the opaque body to send. See config.Warm for why it exists.
type WarmSpec struct {
	Host   string
	Port   int
	Method string
	Path   string
	Body   string
}

// ResolveWarmSpec fills in a per-server warm spec from config.Warm,
// defaulting Host and Port to the server's own values, exactly as
// ResolveSpec does for the health gate.
func ResolveWarmSpec(w config.Warm, serverHost string, serverPort int) WarmSpec {
	spec := WarmSpec{Host: w.Host, Port: w.Port, Method: w.Method, Path: w.Path, Body: w.Body}
	if spec.Host == "" {
		spec.Host = serverHost
	}
	if spec.Port == 0 {
		spec.Port = serverPort
	}
	if spec.Method == "" {
		spec.Method = http.MethodPost
	}
	return spec
}

// URL renders the spec as the request target.
func (s WarmSpec) URL() string {
	return fmt.Sprintf("http://%s:%d%s", s.Host, s.Port, s.Path)
}

// ProbeWarm issues the warm-up request and reports whether it returned 2xx.
// Only the status code is examined; the body is drained and discarded.
//
// It takes no per-attempt timeout, unlike Probe: a warm-up's whole purpose is
// to block for however long the server needs to become able to answer (a cold
// 30GB mlx weight-load is tens of seconds), so bounding it with the
// health-probe timeout would abandon exactly the wait it exists to perform.
// The caller's ctx carries the only deadline.
func ProbeWarm(ctx context.Context, spec WarmSpec) Result {
	var body io.Reader
	if spec.Body != "" {
		body = strings.NewReader(spec.Body)
	}

	req, err := http.NewRequestWithContext(ctx, spec.Method, spec.URL(), body)
	if err != nil {
		return Result{Err: err, Rendered: spec.Path}
	}
	if spec.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{Err: err, Rendered: spec.Path}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	return Result{
		Healthy:    resp.StatusCode >= 200 && resp.StatusCode < 300,
		StatusCode: resp.StatusCode,
		Rendered:   fmt.Sprintf("%s %d", spec.Path, resp.StatusCode),
	}
}

// WarmConfig configures the warm-up attempt loop.
type WarmConfig struct {
	Spec WarmSpec

	// PollInterval is the delay before retrying a warm-up that failed
	// outright (the server has not opened its listener yet).
	PollInterval time.Duration
	// Timeout bounds every attempt together — the server's resolved
	// ready-timeout, which must cover the work the warm-up provokes.
	Timeout time.Duration

	ServerName string
	LogPath    string
}

// Warm sends the warm-up request, retrying until it returns 2xx or
// cfg.Timeout elapses. Retries exist for the gap between launch and the
// server's listener opening, where the request fails immediately with a
// connect error; a request that is merely slow is left alone to finish.
func Warm(ctx context.Context, cfg WarmConfig) (Result, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	for {
		result := ProbeWarm(deadlineCtx, cfg.Spec)
		if result.Healthy {
			return result, nil
		}
		if isWarmRequestRejected(result.StatusCode) {
			// The server understood the request and refused it: the warm-up
			// string is wrong (a path that does not exist, a body this server
			// will not accept). Retrying cannot fix a config error, and doing
			// so for the whole ready-timeout would make an operator wait
			// minutes for a diagnosis the first response already gave.
			return result, fmt.Errorf(
				"server %q: warm-up %s %s returned %d — the request is rejected, not pending; check the `warm` string in the config. See log: %s",
				cfg.ServerName, cfg.Spec.Method, cfg.Spec.Path, result.StatusCode, cfg.LogPath)
		}

		select {
		case <-deadlineCtx.Done():
			return result, fmt.Errorf(
				"server %q: warm-up %s %s did not return 2xx within %s — see log: %s",
				cfg.ServerName, cfg.Spec.Method, cfg.Spec.Path, cfg.Timeout, cfg.LogPath)
		case <-time.After(cfg.PollInterval):
		}
	}
}

// isWarmRequestRejected reports whether status means the server understood the
// warm-up request and will not honour it, however long we keep asking.
//
// 4xx is the client's fault, so retrying is pointless -- except for the two
// that explicitly mean "later": 408 (the server timed out reading it) and 429
// (rate limited) are transient by definition. 5xx is retried, since a server
// still loading may well answer 500 or 503 first. Connect errors carry status
// 0 and are retried: that is the window before the listener opens, which is
// the reason retries exist at all.
func isWarmRequestRejected(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return status >= 400 && status < 500
}

// PollConfig configures a readiness poll loop.
type PollConfig struct {
	Spec Spec

	// ProbeTimeout bounds each individual GET (the system-wide
	// [supervisor] health_timeout, or a caller-resolved override).
	ProbeTimeout time.Duration
	// PollInterval is the delay between retries after a failed probe.
	PollInterval time.Duration
	// ReadyTimeout bounds the whole poll loop (per-server override of the
	// supervisor default, e.g. 90s for cold mlx weight loads).
	ReadyTimeout time.Duration

	// ServerName and LogPath are carried into the timeout error so a
	// failure is loud and points at where to look.
	ServerName string
	LogPath    string
}

// PollReady probes spec repeatedly until it reports healthy or
// cfg.ReadyTimeout elapses. The first probe is always attempted regardless
// of how cfg.ReadyTimeout compares to cfg.PollInterval — only the retry
// cadence between attempts is gated by PollInterval. Returns the last
// Result and a nil error on success; on timeout or context cancellation,
// returns a non-nil error naming the server and its log path.
func PollReady(ctx context.Context, cfg PollConfig) (Result, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, cfg.ReadyTimeout)
	defer cancel()

	result := Probe(deadlineCtx, cfg.Spec, cfg.ProbeTimeout)
	if result.Healthy {
		return result, nil
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadlineCtx.Done():
			return result, fmt.Errorf(
				"server %q did not become healthy within %s — see log: %s",
				cfg.ServerName, cfg.ReadyTimeout, cfg.LogPath)
		case <-ticker.C:
			result = Probe(deadlineCtx, cfg.Spec, cfg.ProbeTimeout)
			if result.Healthy {
				return result, nil
			}
		}
	}
}
