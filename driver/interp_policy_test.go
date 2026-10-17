package driver

import (
	"context"
	"reflect"
	"testing"

	"github.com/iansmith/aatoolkit/host"
	"github.com/iansmith/aatoolkit/interp"
)

// policyInject is what a consumer injects for the host package: the Host
// interface every policy names, plus ContextSender and ContextBuilder, which
// recipe R2 needs to assert -- an interface the policy declared itself would
// not do.
var policyInject = interp.Inject{
	"github.com/iansmith/aatoolkit/host/host": {
		"Host":           reflect.ValueOf((*host.Host)(nil)),
		"ContextSender":  reflect.ValueOf((*host.ContextSender)(nil)),
		"ContextBuilder": reflect.ValueOf((*host.ContextBuilder)(nil)),
	},
}

// hostOnlyInject is a consumer's Inject from before ContextSender existed:
// the Host interface alone. Recipe R1 asks for no change to it.
var hostOnlyInject = interp.Inject{
	"github.com/iansmith/aatoolkit/host/host": {
		"Host": reflect.ValueOf((*host.Host)(nil)),
	},
}

// TestPolicy_ReachesContextFormsThroughInterp: an interpreted policy, loaded
// with the real interp.Load, sends under the caller's context both ways the
// release notes describe. R1: the policy is unchanged and the caller hands it
// a WithContext view; that works with the consumer's Inject unchanged too,
// with Host alone and no ContextSender. R2: the policy takes a ctx and
// asserts host.ContextSender. Either way the upstream request carries the
// caller's context value; an unbound host, or a context without the value,
// carries none.
func TestPolicy_ReachesContextFormsThroughInterp(t *testing.T) {
	window := []byte(`[{"role":"user","content":"hi"}]`)

	r1 := loadPolicy[func(host.Host, []byte) (string, error)](t, "testdata/boundpolicy", policyInject, "policy.Turn")
	r1HostOnly := loadPolicy[func(host.Host, []byte) (string, error)](t, "testdata/boundpolicy", hostOnlyInject, "policy.Turn")
	r2 := loadPolicy[func(context.Context, host.Host, []byte) (string, error)](t, "testdata/ctxpolicy", policyInject, "policy.Turn")

	cases := []struct {
		name string
		turn func(h *Host) (string, error)
		want any
	}{
		{"R1 WithContext view", func(h *Host) (string, error) { return r1(h.WithContext(withCtxValue("policy")), window) }, "policy"},
		{"R1 unbound host", func(h *Host) (string, error) { return r1(h, window) }, nil},
		{"R1 WithContext view, Host-only Inject", func(h *Host) (string, error) {
			return r1HostOnly(h.WithContext(withCtxValue("policy")), window)
		}, "policy"},
		{"R1 unbound host, Host-only Inject", func(h *Host) (string, error) { return r1HostOnly(h, window) }, nil},
		{"R2 explicit context", func(h *Host) (string, error) { return r2(withCtxValue("policy"), h, window) }, "policy"},
		{"R2 context without the value", func(h *Host) (string, error) { return r2(context.Background(), h, window) }, nil},
		{"R2 explicit context on a bound view", func(h *Host) (string, error) {
			return r2(withCtxValue("explicit"), h.WithContext(withCtxValue("bound")), window)
		}, "explicit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, rt := newCtxRecordingHost()
			got, err := tc.turn(h)
			if err != nil {
				t.Fatalf("policy turn: %v", err)
			}
			if got != "ok" {
				t.Errorf("policy turn returned %q, want %q", got, "ok")
			}
			calls, _, _, value := rt.last()
			if calls != 1 {
				t.Fatalf("upstream saw %d requests, want 1", calls)
			}
			if value != tc.want {
				t.Errorf("request carried context value %v, want %v", value, tc.want)
			}
		})
	}
}

// loadPolicy loads sym from the interpreted policy in dir with inject, and
// fails t unless it has type F.
func loadPolicy[F any](t *testing.T, dir string, inject interp.Inject, sym string) F {
	t.Helper()
	v, err := interp.Load(dir, "policy", inject, sym)
	if err != nil {
		t.Fatalf("Load %s %s: %v", dir, sym, err)
	}
	f, ok := v.Interface().(F)
	if !ok {
		t.Fatalf("%s %s has type %T", dir, sym, v.Interface())
	}
	return f
}

// TestPolicy_ReachesContextForThroughInterp: an interpreted policy, loaded with
// the real interp.Load, builds its window under the caller's context both ways
// the README describes, and that ctx reaches the UserContextCtx provider. R1:
// the policy calls Context on a WithContext view, with the consumer's Inject
// with or without ContextBuilder. R2: the policy takes a ctx and asserts
// host.ContextBuilder. An unbound host hands the provider no value.
func TestPolicy_ReachesContextForThroughInterp(t *testing.T) {
	r1 := loadPolicy[func(host.Host, string) []byte](t, "testdata/boundpolicy", policyInject, "policy.Window")
	r1HostOnly := loadPolicy[func(host.Host, string) []byte](t, "testdata/boundpolicy", hostOnlyInject, "policy.Window")
	r2 := loadPolicy[func(context.Context, host.Host, string) ([]byte, error)](t, "testdata/ctxpolicy", policyInject, "policy.Window")

	cases := []struct {
		name   string
		window func(h *Host) ([]byte, error)
		want   any
	}{
		{"R1 WithContext view", func(h *Host) ([]byte, error) { return r1(h.WithContext(withCtxValue("policy")), "q"), nil }, "policy"},
		{"R1 unbound host", func(h *Host) ([]byte, error) { return r1(h, "q"), nil }, nil},
		{"R1 WithContext view, Host-only Inject", func(h *Host) ([]byte, error) {
			return r1HostOnly(h.WithContext(withCtxValue("policy")), "q"), nil
		}, "policy"},
		{"R2 explicit context", func(h *Host) ([]byte, error) { return r2(withCtxValue("policy"), h, "q") }, "policy"},
		{"R2 explicit context on a bound view", func(h *Host) ([]byte, error) {
			return r2(withCtxValue("explicit"), h.WithContext(withCtxValue("bound")), "q")
		}, "explicit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, p := newProbeHost()
			got, err := tc.window(h)
			if err != nil {
				t.Fatalf("policy Window: %v", err)
			}
			p.assertOnce(t, tc.want, "q")
			assertSystemMessages(t, got, "sys", "ctx block")
		})
	}
}

// TestPolicy_R2BuildsAndSendsUnderOneCtx: the whole R2 recipe the README gives,
// run by one interpreted policy -- ContextFor, then SendStreamContext, under
// the one ctx it is handed -- carries that ctx to both the user-context
// provider and the upstream request.
func TestPolicy_R2BuildsAndSendsUnderOneCtx(t *testing.T) {
	reply := loadPolicy[func(context.Context, host.Host, string) (string, error)](t, "testdata/ctxpolicy", policyInject, "policy.Reply")

	h, rt := newCtxRecordingHost()
	p := &ctxProbe{result: "ctx block"}
	h.userContext = p.provide

	got, err := reply(withCtxValue("policy"), h, "q")
	if err != nil {
		t.Fatalf("policy Reply: %v", err)
	}
	if got != "ok" {
		t.Errorf("policy Reply returned %q, want %q", got, "ok")
	}
	p.assertOnce(t, "policy", "q")
	if calls, _, _, value := rt.last(); calls != 1 || value != "policy" {
		t.Errorf("upstream saw %d requests carrying %v; want 1 carrying %q", calls, value, "policy")
	}
}
