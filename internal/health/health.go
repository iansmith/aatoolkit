// Package health implements the mandatory health gate for aa-server-status
// servers (design/aa-server-status.md §6.1 stage 3, §7.1) in the two forms a
// server may declare it: a single GET request whose 2xx response is the
// authoritative "serving" signal, or a command whose exit status of 0 is. Every
// server declares exactly one; config.Validate rejects both and neither.
//
// Neither form has a fallback to a lesser signal. Connect errors, timeouts,
// non-2xx responses, and non-zero exits are all simply "not healthy."
package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/iansmith/aatoolkit/config"
)

// Spec is a fully resolved health-check target, in one of the two forms
// config.Health allows: an HTTP GET (Host, Port, Path), or a command to run
// (Exec). Unlike config.Health, the HTTP form's fields are guaranteed
// populated. config.Validate has already rejected a spec declaring both forms
// or neither, so exactly one is set here.
type Spec struct {
	Host string
	Port int
	Path string

	// Exec, when non-empty, is the argv of a probe program whose exit status
	// is the readiness signal. It is never passed to a shell.
	Exec []string
}

// IsExec reports which form this spec is, mirroring config.Health.IsExec.
func (s Spec) IsExec() bool { return len(s.Exec) > 0 }

// ResolveSpec fills in a per-server health spec from config.Health,
// defaulting Host and Port to the server's own values when left unset in
// the TOML. Path has no default — config.Validate already enforces that one
// of the two forms is populated before a Health value reaches here.
//
// The exec form carries no host or port to default (config.Validate rejects
// them as meaningless for a command), so the defaulting below is inert for it
// rather than needing a branch of its own.
func ResolveSpec(h config.Health, serverHost string, serverPort int) Spec {
	spec := Spec{Host: h.Host, Port: h.Port, Path: h.Path, Exec: h.Exec}
	if spec.IsExec() {
		return spec
	}
	if spec.Host == "" {
		spec.Host = serverHost
	}
	if spec.Port == 0 {
		spec.Port = serverPort
	}
	return spec
}

// SpecFor resolves s's health spec for a caller that wants it optionally —
// nil when the server declares no check at all. Both teardown paths need
// exactly this, so it lives here rather than being spelled out identically in
// each of them; a third health form then reaches both by changing Declared,
// not by being threaded through two packages.
func SpecFor(s config.Server) *Spec {
	if !s.Health.Declared() {
		return nil
	}
	spec := ResolveSpec(s.Health, s.Host, s.Port)
	return &spec
}

// URL renders the spec as the GET target. HTTP form only.
func (s Spec) URL() string {
	return fmt.Sprintf("http://%s:%d%s", s.Host, s.Port, s.Path)
}

// Describe renders the spec for an error message, in whichever form it is.
// Callers reporting a probe failure use this rather than URL, which would
// render an exec spec as the meaningless "http://:0".
func (s Spec) Describe() string {
	if s.IsExec() {
		return fmt.Sprintf("exec %s", strings.Join(s.Exec, " "))
	}
	return fmt.Sprintf("GET %s", s.URL())
}

// Result is the outcome of a single health probe.
type Result struct {
	// Healthy is true only when the GET completed with a 2xx status, or the
	// exec probe exited 0.
	Healthy bool
	// StatusCode is the HTTP response status, or 0 if no response was
	// received (connect error, timeout, or other request failure). It is
	// always 0 for an exec probe — an exit status is not an HTTP status, and
	// conflating the two would render "exit 1" as a bad request.
	StatusCode int
	// Err is the request-level error on failure to get any response, or the
	// exec probe's non-zero exit / failure to run at all.
	Err error
	// Rendered is the status-table form, e.g. "/v1/models 200" on a
	// completed request, or just the path if no status was received; for an
	// exec probe, the program name and its exit status.
	Rendered string
	// Output is an exec probe's combined stdout and stderr, trimmed and
	// capped at maxProbeOutput. A probe that fails silently is as useless as
	// no probe, and its output goes nowhere else: the probe is a separate
	// process, so the supervised server's own log never sees it. Always empty
	// for the HTTP form, whose diagnosis is the status code.
	Output string
}

// maxProbeOutput caps how much of a probe's output is kept. The timeout bounds
// how long a probe may run, not how much it may write, so a probe that chatters
// — a wrapper echoing progress, or one stuck in a log loop — would otherwise
// buffer the whole of it into supervisor memory, on every poll tick. A
// readiness diagnosis is a line or two; anything past this is noise being paid
// for in RSS.
const maxProbeOutput = 4 << 10

// cappedBuffer accumulates up to maxProbeOutput bytes and silently drops the
// rest. It keeps the head rather than the tail: a probe that fails says why
// first and then repeats itself, so the opening bytes are the diagnosis.
type cappedBuffer struct {
	buf      []byte
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := maxProbeOutput - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(room, len(p))]...)
	}
	if len(c.buf) == maxProbeOutput {
		c.overflow = true
	}
	// Always report a full write: a short count would make the probe program
	// see a write error on its own stdout, changing the behaviour we are
	// trying to observe.
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	s := string(bytes.TrimSpace(c.buf))
	if c.overflow {
		return s + " […truncated]"
	}
	return s
}

// Probe runs a single health check against spec, bounded by timeout, and
// reports whether the server is serving. There is no fallback to a lesser
// signal: for the HTTP form a 2xx response is the only healthy outcome, and
// for the exec form an exit status of 0 is.
func Probe(ctx context.Context, spec Spec, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if spec.IsExec() {
		return probeExec(ctx, spec, timeout)
	}

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

// probeExec runs spec.Exec to completion under ctx and reports exit 0 as ready.
// The supervisor learns nothing about the protocol behind the server — it runs
// a program and reads a status code (design/aa-server-status.md §6.1).
//
// ctx already carries the timeout; WaitDelay bounds the second way this can
// hang. Killing the process on ctx expiry does not, on its own, unblock the
// read of its output: a grandchild that inherited the pipes keeps them open for
// its own lifetime, so a probe whose process exited in milliseconds could still
// hold the supervisor for as long as something it spawned lives. WaitDelay
// gives that drain the same budget the probe itself got, then abandons it.
func probeExec(ctx context.Context, spec Spec, timeout time.Duration) Result {
	var out cappedBuffer

	cmd := exec.CommandContext(ctx, spec.Exec[0], spec.Exec[1:]...)
	cmd.WaitDelay = timeout
	cmd.Stdout, cmd.Stderr = &out, &out

	err := cmd.Run()
	result := Result{
		Err:    err,
		Output: out.String(),
	}

	// A non-zero exit is the probe doing its job, so it reports "not ready"
	// with the exit status; anything else (program not found, not executable,
	// killed at the timeout) has no status to report and is rendered as the
	// failure it is.
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.Healthy = true
		result.Rendered = fmt.Sprintf("%s 0", spec.Exec[0])
	case errors.As(err, &exitErr):
		result.Rendered = fmt.Sprintf("%s exit %d", spec.Exec[0], exitErr.ExitCode())
	default:
		result.Rendered = fmt.Sprintf("%s failed", spec.Exec[0])
	}
	return result
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

	// explanation is the most recent attempt that actually said something,
	// which is not always the most recent attempt: whichever probe is in
	// flight when the ready-timeout expires is killed mid-run and comes back
	// empty. Reporting that one would throw away the diagnosis every earlier
	// attempt gave.
	//
	// It is worth carrying at all because the log path the error already
	// names is the *server's* log, and an exec probe never writes there — it
	// is a separate process. For that form, the log the error points at is
	// the one place the explanation is guaranteed not to be.
	var result Result
	var explanation string
	attempt := func() bool {
		result = Probe(deadlineCtx, cfg.Spec, cfg.ProbeTimeout)
		if result.Output != "" {
			explanation = result.Output
		}
		return result.Healthy
	}

	if attempt() {
		return result, nil
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadlineCtx.Done():
			suffix := ""
			if explanation != "" {
				suffix = "\nlast probe output: " + explanation
			}
			return result, fmt.Errorf(
				"server %q did not become healthy within %s — see log: %s%s",
				cfg.ServerName, cfg.ReadyTimeout, cfg.LogPath, suffix)
		case <-ticker.C:
			if attempt() {
				return result, nil
			}
		}
	}
}
