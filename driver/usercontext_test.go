package driver

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/host"
)

// The interpreted policy only ever sees host.Host, so the driver must satisfy
// it exactly — including Context's query-text parameter.
var _ host.Host = (*Host)(nil)

// testTiers points at a dead endpoint: these tests only assemble context.
var testTiers = map[string]Tier{"fast": {URL: "http://127.0.0.1:1", Model: "test", MaxTokens: 512}}

// assertSystemMessages checks that ctx is exactly the given system messages,
// in order.
func assertSystemMessages(t *testing.T, ctx []byte, want ...string) {
	t.Helper()
	var msgs []message
	if err := json.Unmarshal(ctx, &msgs); err != nil {
		t.Fatalf("failed to unmarshal context: %v", err)
	}
	if len(msgs) != len(want) {
		t.Fatalf("want %d messages, got %d: %+v", len(want), len(msgs), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != "system" || msgs[i].Content != w {
			t.Fatalf("message %d: want {role: system, content: %q}, got {role: %s, content: %q}",
				i, w, msgs[i].Role, msgs[i].Content)
		}
	}
}

// TestDriver_UserContextInjected verifies that when UserContext is provided,
// it is injected as a system message immediately after the system prompt
// and before history.
func TestDriver_UserContextInjected(t *testing.T) {
	systemPrompt := "You are a helpful assistant."
	userContextBlock := "The current user is Ian."

	h := &Host{
		client:      &http.Client{},
		prompt:      func() string { return systemPrompt },
		tiers:       testTiers,
		userContext: func(context.Context, string) string { return userContextBlock },
		history:     []message{},
	}

	assertSystemMessages(t, h.Context(""), systemPrompt, userContextBlock)
}

// TestDriver_UserContextNilUnchanged verifies that when UserContext is nil,
// the context is byte-identical to the system-prompt-then-history assembly.
func TestDriver_UserContextNilUnchanged(t *testing.T) {
	systemPrompt := "You are a helpful assistant."

	h := &Host{
		client:      &http.Client{},
		prompt:      func() string { return systemPrompt },
		tiers:       testTiers,
		userContext: nil,
		history:     []message{},
	}

	assertSystemMessages(t, h.Context(""), systemPrompt)
}

// TestDriver_UserContextWiredThroughNew verifies that Config.UserContext, as
// passed to the public driver.New constructor, is actually wired into the
// Host used by Context() — not just the unexported field when set directly —
// and that the text passed to Context reaches it, so a consumer can build a
// query-dependent block (e.g. retrieval keyed on the current turn).
func TestDriver_UserContextWiredThroughNew(t *testing.T) {
	systemPrompt := "You are a helpful assistant."

	h := New(Config{
		Tiers:       testTiers,
		Prompt:      func() string { return systemPrompt },
		UserContext: func(q string) string { return "context for " + q },
	})

	assertSystemMessages(t, h.Context("what did I say yesterday?"),
		systemPrompt, "context for what did I say yesterday?")
}

// TestDriver_UserContextRunsOutsideHistoryLock verifies that a slow
// UserContext provider (e.g. retrieval I/O) does not hold the history lock:
// Remember must complete while the provider is still blocked inside Context.
func TestDriver_UserContextRunsOutsideHistoryLock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := New(Config{
		Tiers:  testTiers,
		Prompt: func() string { return "sys" },
		UserContext: func(string) string {
			close(entered)
			<-release
			return ""
		},
	})

	done := make(chan struct{})
	go func() { h.Context("q"); close(done) }()
	<-entered

	remembered := make(chan struct{})
	go func() { h.Remember("user", []byte("hi")); close(remembered) }()
	select {
	case <-remembered:
	case <-time.After(time.Second):
		t.Fatal("Remember blocked while the UserContext provider was running")
	}
	close(release)
	<-done
}

// ctxProbe is a UserContextCtx provider that records each call: the ctx it
// is handed and the query text.
type ctxProbe struct {
	mu     sync.Mutex
	call   probeCall
	result string
}

// probeCall is what a ctxProbe saw: how many calls, and the last call's ctx
// and query text.
type probeCall struct {
	calls int
	ctx   context.Context
	query string
}

// value is the value under ctxKey on the last call's ctx, or nil.
func (c probeCall) value() any {
	if c.ctx == nil {
		return nil
	}
	return c.ctx.Value(ctxKey{})
}

func (p *ctxProbe) provide(ctx context.Context, q string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.call = probeCall{calls: p.call.calls + 1, ctx: ctx, query: q}
	return p.result
}

func (p *ctxProbe) last() probeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call
}

// assertOnce fails t unless the probe was called exactly once, with a ctx
// carrying wantValue under ctxKey and with wantQuery.
func (p *ctxProbe) assertOnce(t *testing.T, wantValue any, wantQuery string) {
	t.Helper()
	if c := p.last(); c.calls != 1 || c.value() != wantValue || c.query != wantQuery {
		t.Fatalf("provider saw calls=%d value=%v query=%q; want 1, %v, %q", c.calls, c.value(), c.query, wantValue, wantQuery)
	}
}

// newProbeHost returns a Host whose system prompt is "sys" and whose
// UserContextCtx is a ctxProbe answering "ctx block", and that probe.
func newProbeHost() (*Host, *ctxProbe) {
	p := &ctxProbe{result: "ctx block"}
	return New(Config{Tiers: testTiers, Prompt: func() string { return "sys" }, UserContextCtx: p.provide}), p
}

// TestContextFor_PassesCtxToProvider: the ctx handed to ContextFor reaches the
// UserContextCtx provider, along with the query text, and the provider's block
// is injected after the system prompt.
func TestContextFor_PassesCtxToProvider(t *testing.T) {
	h, p := newProbeHost()

	got := h.ContextFor(withCtxValue("turn"), "q1")

	p.assertOnce(t, "turn", "q1")
	assertSystemMessages(t, got, "sys", "ctx block")
}

// TestContextFor_NilCtxPanics: a nil ctx panics in ContextFor itself, as
// WithContext(nil) does, rather than reaching the provider -- whichever
// provider form is configured.
func TestContextFor_NilCtxPanics(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"UserContextCtx", Config{UserContextCtx: func(ctx context.Context, _ string) string { return ctx.Value(ctxKey{}).(string) }}},
		{"UserContext", Config{UserContext: func(string) string { return "block" }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Tiers, tc.cfg.Prompt = testTiers, func() string { return "sys" }
			h := New(tc.cfg)
			defer func() {
				if r := recover(); r != "driver: ContextFor called with a nil context" {
					t.Errorf("ContextFor(nil) recovered %v; want the driver's nil-context panic", r)
				}
			}()
			var nilCtx context.Context
			h.ContextFor(nilCtx, "q")
		})
	}
}

// TestUserContext_BothSetCtxWins: with both providers configured, Context and
// ContextFor call only UserContextCtx; UserContext is never called.
func TestUserContext_BothSetCtxWins(t *testing.T) {
	var plainCalls atomic.Int32
	p := &ctxProbe{result: "ctx block"}
	h := New(Config{
		Tiers:          testTiers,
		Prompt:         func() string { return "sys" },
		UserContext:    func(string) string { plainCalls.Add(1); return "plain block" },
		UserContextCtx: p.provide,
	})

	assertSystemMessages(t, h.Context("q"), "sys", "ctx block")
	assertSystemMessages(t, h.ContextFor(withCtxValue("turn"), "q"), "sys", "ctx block")

	if n := plainCalls.Load(); n != 0 {
		t.Errorf("UserContext called %d times; want 0 when UserContextCtx is set", n)
	}
	if c := p.last(); c.calls != 2 {
		t.Errorf("UserContextCtx called %d times; want 2", c.calls)
	}
}

// TestContext_PassesBackgroundToCtxProvider: Context, which takes no ctx,
// hands a UserContextCtx provider context.Background().
func TestContext_PassesBackgroundToCtxProvider(t *testing.T) {
	h, p := newProbeHost()

	assertSystemMessages(t, h.Context("q"), "sys", "ctx block")

	if c := p.last(); c.calls != 1 || c.ctx != context.Background() {
		t.Fatalf("provider saw calls=%d ctx=%v; want 1 call under context.Background()", c.calls, c.ctx)
	}
}

// TestWithContext_ContextUsesBoundCtx: on the WithContext view, Context hands
// the provider the bound ctx, and ContextFor hands it the explicit one.
func TestWithContext_ContextUsesBoundCtx(t *testing.T) {
	h, p := newProbeHost()
	v := h.WithContext(withCtxValue("bound"))

	assertSystemMessages(t, v.Context("q"), "sys", "ctx block")
	if got := p.last().value(); got != "bound" {
		t.Errorf("view's Context handed the provider %v; want the bound ctx's %q", got, "bound")
	}

	cb, ok := v.(host.ContextBuilder)
	if !ok {
		t.Fatal("the WithContext view is not a host.ContextBuilder")
	}
	cb.ContextFor(withCtxValue("explicit"), "q")
	if got := p.last().value(); got != "explicit" {
		t.Errorf("view's ContextFor handed the provider %v; want the explicit ctx's %q", got, "explicit")
	}
}
