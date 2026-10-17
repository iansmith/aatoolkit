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

// The context-taking form of Context is reachable from both the Host and the
// view WithContext returns.
var (
	_ host.ContextBuilder = (*Host)(nil)
	_ host.ContextBuilder = (*boundHost)(nil)
)

// ctxProbe is a UserContextCtx provider that records the value under ctxKey
// on each ctx it is handed, the ctx itself, and the query text.
type ctxProbe struct {
	mu     sync.Mutex
	calls  int
	ctx    context.Context
	value  any
	query  string
	result string
}

func (p *ctxProbe) provide(ctx context.Context, q string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.ctx, p.value, p.query = ctx, ctx.Value(ctxKey{}), q
	return p.result
}

func (p *ctxProbe) last() (int, context.Context, any, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.ctx, p.value, p.query
}

func withCtxValue(v string) context.Context {
	return context.WithValue(context.Background(), ctxKey{}, v)
}

// TestContextFor_PassesCtxToProvider: the ctx handed to ContextFor reaches the
// UserContextCtx provider, along with the query text, and the provider's block
// is injected after the system prompt.
func TestContextFor_PassesCtxToProvider(t *testing.T) {
	p := &ctxProbe{result: "ctx block"}
	h := New(Config{Tiers: testTiers, Prompt: func() string { return "sys" }, UserContextCtx: p.provide})

	got := h.ContextFor(withCtxValue("turn"), "q1")

	calls, _, v, q := p.last()
	if calls != 1 || v != "turn" || q != "q1" {
		t.Fatalf("provider saw calls=%d value=%v query=%q; want 1, %q, %q", calls, v, q, "turn", "q1")
	}
	assertSystemMessages(t, got, "sys", "ctx block")
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
	if calls, _, _, _ := p.last(); calls != 2 {
		t.Errorf("UserContextCtx called %d times; want 2", calls)
	}
}

// TestContext_PassesBackgroundToCtxProvider: Context, which takes no ctx,
// hands a UserContextCtx provider context.Background().
func TestContext_PassesBackgroundToCtxProvider(t *testing.T) {
	p := &ctxProbe{result: "ctx block"}
	h := New(Config{Tiers: testTiers, Prompt: func() string { return "sys" }, UserContextCtx: p.provide})

	assertSystemMessages(t, h.Context("q"), "sys", "ctx block")

	if calls, ctx, _, _ := p.last(); calls != 1 || ctx != context.Background() {
		t.Fatalf("provider saw calls=%d ctx=%v; want 1 call under context.Background()", calls, ctx)
	}
}

// TestWithContext_ContextUsesBoundCtx: on the WithContext view, Context hands
// the provider the bound ctx, and ContextFor hands it the explicit one.
func TestWithContext_ContextUsesBoundCtx(t *testing.T) {
	p := &ctxProbe{result: "ctx block"}
	h := New(Config{Tiers: testTiers, Prompt: func() string { return "sys" }, UserContextCtx: p.provide})
	v := h.WithContext(withCtxValue("bound"))

	assertSystemMessages(t, v.Context("q"), "sys", "ctx block")
	if _, _, got, _ := p.last(); got != "bound" {
		t.Errorf("view's Context handed the provider %v; want the bound ctx's %q", got, "bound")
	}

	cb, ok := v.(host.ContextBuilder)
	if !ok {
		t.Fatal("the WithContext view is not a host.ContextBuilder")
	}
	cb.ContextFor(withCtxValue("explicit"), "q")
	if _, _, got, _ := p.last(); got != "explicit" {
		t.Errorf("view's ContextFor handed the provider %v; want the explicit ctx's %q", got, "explicit")
	}
}
