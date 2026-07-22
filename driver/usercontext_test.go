package driver

import (
	"encoding/json"
	"net/http"
	"testing"
)

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
		userContext: func() string { return userContextBlock },
		history:     []message{},
	}

	ctx := h.Context()

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

	ctx := h.Context()

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
