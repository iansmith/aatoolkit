package main

import (
	"bufio"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// TestAskPrompts_AppliesBranchEnvToTheCopyNotTheInput pins AATK-32 observable
// behavior 5, and it is the one case the engine-level tests cannot see.
//
// askPrompts does `copy(resolved, targets)` — a shallow copy, so every
// resolved[i].Env is the same map header as targets[i].Env. Merging the chosen
// branch into it in place would work perfectly from the child's point of view
// while quietly rewriting the caller's config, and a reload or a second Up
// would then read the mutated version. The args path avoids this by building a
// fresh slice; the env path has to be equally deliberate.
//
// Both halves matter: the resolved copy must carry the branch value (the
// feature), and the input map must still hold the static one (the aliasing bug).
func TestAskPrompts_AppliesBranchEnvToTheCopyNotTheInput(t *testing.T) {
	const key = "AATOOLKIT_PROMPT_ENV_PROBE"

	input := []config.Server{{
		Name:    "svc",
		Type:    config.TypeExec,
		Command: "/bin/true",
		Env:     map[string]string{key: "static"},
		Prompt: &config.PromptSpec{
			Question: "Use the local endpoint for this run?",
			YesEnv:   map[string]string{key: "chosen"},
		},
	}}
	// Captured before resolution: asserting on input[0].Env afterwards would
	// pass even if askPrompts replaced the whole Server value.
	inputEnv := input[0].Env

	var promptOut strings.Builder
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t)},
		bufio.NewReader(strings.NewReader("y\n")), &promptOut)

	resolved, err := eng.askPrompts(input)
	if err != nil {
		t.Fatalf("askPrompts: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("askPrompts returned %d servers, want 1", len(resolved))
	}

	if got := resolved[0].Env[key]; got != "chosen" {
		t.Errorf("resolved Env[%s] = %q, want %q — the chosen branch's env must be applied", key, got, "chosen")
	}
	if got := inputEnv[key]; got != "static" {
		t.Errorf("input Env[%s] = %q, want %q — resolution must not mutate the caller's map", key, got, "static")
	}
}
