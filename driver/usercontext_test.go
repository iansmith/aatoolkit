package driver

import (
	"encoding/json"
	"net/http"
	"testing"

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
		userContext: func(string) string { return userContextBlock },
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
