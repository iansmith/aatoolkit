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

	v, err := interp.Load("testdata/boundpolicy", "policy", policyInject, "policy.Turn")
	if err != nil {
		t.Fatalf("Load boundpolicy: %v", err)
	}
	r1, ok := v.Interface().(func(host.Host, []byte) (string, error))
	if !ok {
		t.Fatalf("boundpolicy.Turn has type %T", v.Interface())
	}

	v, err = interp.Load("testdata/boundpolicy", "policy", hostOnlyInject, "policy.Turn")
	if err != nil {
		t.Fatalf("Load boundpolicy with a Host-only Inject: %v", err)
	}
	r1HostOnly, ok := v.Interface().(func(host.Host, []byte) (string, error))
	if !ok {
		t.Fatalf("boundpolicy.Turn with a Host-only Inject has type %T", v.Interface())
	}

	v, err = interp.Load("testdata/ctxpolicy", "policy", policyInject, "policy.Turn")
	if err != nil {
		t.Fatalf("Load ctxpolicy: %v", err)
	}
	r2, ok := v.Interface().(func(context.Context, host.Host, []byte) (string, error))
	if !ok {
		t.Fatalf("ctxpolicy.Turn has type %T", v.Interface())
	}

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

// TestPolicy_ReachesContextForThroughInterp: an interpreted policy, loaded with
// the real interp.Load and an Inject carrying ContextBuilder, builds its window
// under the ctx it is given by asserting host.ContextBuilder, and that ctx
// reaches the UserContextCtx provider.
func TestPolicy_ReachesContextForThroughInterp(t *testing.T) {
	v, err := interp.Load("testdata/ctxbuilderpolicy", "policy", policyInject, "policy.Window")
	if err != nil {
		t.Fatalf("Load ctxbuilderpolicy: %v", err)
	}
	window, ok := v.Interface().(func(context.Context, host.Host, string) ([]byte, error))
	if !ok {
		t.Fatalf("ctxbuilderpolicy.Window has type %T", v.Interface())
	}

	p := &ctxProbe{result: "ctx block"}
	h := New(Config{Tiers: testTiers, Prompt: func() string { return "sys" }, UserContextCtx: p.provide})

	got, err := window(withCtxValue("policy"), h, "q")
	if err != nil {
		t.Fatalf("policy Window: %v", err)
	}
	if calls, _, value, q := p.last(); calls != 1 || value != "policy" || q != "q" {
		t.Fatalf("provider saw calls=%d value=%v query=%q; want 1, %q, %q", calls, value, q, "policy", "q")
	}
	assertSystemMessages(t, got, "sys", "ctx block")
}
