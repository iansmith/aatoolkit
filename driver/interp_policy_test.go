package driver

import (
	"context"
	"reflect"
	"testing"

	"github.com/iansmith/aatoolkit/host"
	"github.com/iansmith/aatoolkit/interp"
)

// policyInject is what a consumer injects for the host package: the Host
// interface every policy names, plus ContextSender, which recipe R2 needs to
// assert -- an interface the policy declared itself would not do.
var policyInject = interp.Inject{
	"github.com/iansmith/aatoolkit/host/host": {
		"Host":          reflect.ValueOf((*host.Host)(nil)),
		"ContextSender": reflect.ValueOf((*host.ContextSender)(nil)),
	},
}

// TestPolicy_ReachesContextFormsThroughInterp: an interpreted policy, loaded
// with the real interp.Load, sends under the caller's context both ways the
// release notes describe. R1: the policy is unchanged and the caller hands it
// a WithContext view. R2: the policy takes a ctx and asserts
// host.ContextSender. Either way the upstream request carries the caller's
// context value; an unbound host, or a context without the value, carries
// none.
func TestPolicy_ReachesContextFormsThroughInterp(t *testing.T) {
	window := []byte(`[{"role":"user","content":"hi"}]`)
	withValue := func(v string) context.Context {
		return context.WithValue(context.Background(), ctxKey{}, v)
	}

	v, err := interp.Load("testdata/boundpolicy", "policy", policyInject, "policy.Turn")
	if err != nil {
		t.Fatalf("Load boundpolicy: %v", err)
	}
	r1, ok := v.Interface().(func(host.Host, []byte) (string, error))
	if !ok {
		t.Fatalf("boundpolicy.Turn has type %T", v.Interface())
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
		{"R1 WithContext view", func(h *Host) (string, error) { return r1(h.WithContext(withValue("policy")), window) }, "policy"},
		{"R1 unbound host", func(h *Host) (string, error) { return r1(h, window) }, nil},
		{"R2 explicit context", func(h *Host) (string, error) { return r2(withValue("policy"), h, window) }, "policy"},
		{"R2 context without the value", func(h *Host) (string, error) { return r2(context.Background(), h, window) }, nil},
		{"R2 explicit context on a bound view", func(h *Host) (string, error) {
			return r2(withValue("explicit"), h.WithContext(withValue("bound")), window)
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
