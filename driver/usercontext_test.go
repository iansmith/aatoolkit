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

// TestDriver_UserContextInjected verifies that when UserContext is provided,
// it is injected as a system message immediately after the system prompt
// and before history.
func TestDriver_UserContextInjected(t *testing.T) {
	systemPrompt := "You are a helpful assistant."
	userContextBlock := "The current user is Ian."

	h := &Host{
		client:      &http.Client{},
		prompt:      func() string { return systemPrompt },
		tiers:       map[string]Tier{"fast": {URL: "http://127.0.0.1:1", Model: "test", Reasoning: false, MaxTokens: 512}},
		userContext: func(string) string { return userContextBlock },
		history:     []message{},
	}

	ctx := h.Context("")

	var msgs []message
	if err := json.Unmarshal(ctx, &msgs); err != nil {
		t.Fatalf("failed to unmarshal context: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (system + user context), got %d", len(msgs))
	}

	if msgs[0].Role != "system" || msgs[0].Content != systemPrompt {
		t.Fatalf("first message: want {role: system, content: %q}, got {role: %s, content: %q}",
			systemPrompt, msgs[0].Role, msgs[0].Content)
	}

	if msgs[1].Role != "system" || msgs[1].Content != userContextBlock {
		t.Fatalf("second message: want {role: system, content: %q}, got {role: %s, content: %q}",
			userContextBlock, msgs[1].Role, msgs[1].Content)
	}
}

// TestDriver_UserContextNilUnchanged verifies that when UserContext is nil,
// the context is byte-identical to the system-prompt-then-history assembly.
func TestDriver_UserContextNilUnchanged(t *testing.T) {
	systemPrompt := "You are a helpful assistant."

	h := &Host{
		client:      &http.Client{},
		prompt:      func() string { return systemPrompt },
		tiers:       map[string]Tier{"fast": {URL: "http://127.0.0.1:1", Model: "test", Reasoning: false, MaxTokens: 512}},
		userContext: nil,
		history:     []message{},
	}

	ctx := h.Context("")

	var msgs []message
	if err := json.Unmarshal(ctx, &msgs); err != nil {
		t.Fatalf("failed to unmarshal context: %v", err)
	}

	if len(msgs) != 1 {
		t.Fatalf("want 1 message (system only), got %d", len(msgs))
	}

	if msgs[0].Role != "system" || msgs[0].Content != systemPrompt {
		t.Fatalf("message: want {role: system, content: %q}, got {role: %s, content: %q}",
			systemPrompt, msgs[0].Role, msgs[0].Content)
	}
}

// TestDriver_UserContextWiredThroughNew verifies that Config.UserContext, as
// passed to the public driver.New constructor, is actually wired into the
// Host used by Context() — not just the unexported field when set directly.
func TestDriver_UserContextWiredThroughNew(t *testing.T) {
	systemPrompt := "You are a helpful assistant."
	userContextBlock := "The current user is Ian."

	h := New(Config{
		Tiers:       map[string]Tier{"fast": {URL: "http://127.0.0.1:1", Model: "test", Reasoning: false, MaxTokens: 512}},
		Prompt:      func() string { return systemPrompt },
		UserContext: func(string) string { return userContextBlock },
	})

	ctx := h.Context("")

	var msgs []message
	if err := json.Unmarshal(ctx, &msgs); err != nil {
		t.Fatalf("failed to unmarshal context: %v", err)
	}

	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (system + user context), got %d", len(msgs))
	}

	if msgs[0].Role != "system" || msgs[0].Content != systemPrompt {
		t.Fatalf("first message: want {role: system, content: %q}, got {role: %s, content: %q}",
			systemPrompt, msgs[0].Role, msgs[0].Content)
	}

	if msgs[1].Role != "system" || msgs[1].Content != userContextBlock {
		t.Fatalf("second message: want {role: system, content: %q}, got {role: %s, content: %q}",
			userContextBlock, msgs[1].Role, msgs[1].Content)
	}
}

// TestDriver_UserContextReceivesQueryText verifies that the text passed to
// Context reaches Config.UserContext, so a consumer can build a
// query-dependent block (e.g. retrieval keyed on the current turn).
func TestDriver_UserContextReceivesQueryText(t *testing.T) {
	const query = "what did I say yesterday?"
	var gotQuery string
	h := New(Config{
		Tiers:  map[string]Tier{"fast": {URL: "http://127.0.0.1:1", Model: "test", Reasoning: false, MaxTokens: 512}},
		Prompt: func() string { return "sys" },
		UserContext: func(queryText string) string {
			gotQuery = queryText
			return "context for " + queryText
		},
	})

	ctx := h.Context(query)

	if gotQuery != query {
		t.Fatalf("UserContext got query %q, want %q", gotQuery, query)
	}
	var msgs []message
	if err := json.Unmarshal(ctx, &msgs); err != nil {
		t.Fatalf("failed to unmarshal context: %v", err)
	}
	if len(msgs) != 2 || msgs[1].Content != "context for "+query {
		t.Fatalf("want the user context block built from the query, got %+v", msgs)
	}
}
