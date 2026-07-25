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

// TestAskPrompts_DoesNotMutateThePromptSpecBranchMap closes the second aliasing
// hole, the one the Server.Env test above does not reach.
//
// The natural implementation of "branch wins over static" is to start from the
// branch map and fill in whatever the static env has that the branch lacks:
//
//	for k, v := range s.Env {
//	    if _, ok := chosen[k]; !ok { chosen[k] = v }
//	}
//	resolved[i].Env = chosen
//
// That passes every other test in this suite while permanently folding the
// static keys into the config's own PromptSpec. The failure it ships is a stale
// read after a reload: `up svc` answered yes bakes KEEP=a into spec.YesEnv, a
// reload changes KEEP to b, and the next `up svc` answered yes still launches
// with KEEP=a — the key is now "already present" in the branch, so the fresh
// static value is skipped. The resolved Env would also alias live config state
// that gets handed straight to exec.Cmd.
func TestAskPrompts_DoesNotMutateThePromptSpecBranchMap(t *testing.T) {
	const key = "AATOOLKIT_PROMPT_ENV_PROBE"
	const keepVar = "AATOOLKIT_PROMPT_ENV_KEEP"
	const sentinel = "AATOOLKIT_PROMPT_ENV_SENTINEL"

	spec := &config.PromptSpec{
		Question: "Use the local endpoint for this run?",
		YesEnv:   map[string]string{key: "chosen"},
	}
	input := []config.Server{{
		Name:    "svc",
		Type:    config.TypeExec,
		Command: "/bin/true",
		Env:     map[string]string{key: "static", keepVar: "keep-me"},
		Prompt:  spec,
	}}

	var promptOut strings.Builder
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t)},
		bufio.NewReader(strings.NewReader("y\n")), &promptOut)

	resolved, err := eng.askPrompts(input)
	if err != nil {
		t.Fatalf("askPrompts: %v", err)
	}

	if got := resolved[0].Env[key]; got != "chosen" {
		t.Errorf("resolved Env[%s] = %q, want %q", key, got, "chosen")
	}
	if len(spec.YesEnv) != 1 {
		t.Errorf("prompt spec's YesEnv = %v, want its original single entry — resolution must not fold the static env into the config's own branch map", spec.YesEnv)
	}
	if _, folded := spec.YesEnv[keepVar]; folded {
		t.Errorf("prompt spec's YesEnv gained %s from the static env — a reload would then be shadowed by the stale value", keepVar)
	}

	// The resolved map must not be the spec's map: writing to what the launcher
	// was handed must not reach back into live config.
	resolved[0].Env[sentinel] = "x"
	if _, leaked := spec.YesEnv[sentinel]; leaked {
		t.Errorf("resolved Env aliases the prompt spec's YesEnv map — a write to the launched server's env reached live config")
	}
}

// TestAskPrompts_UndeclaredBranchLeavesStaticEnvIntact is a guard, not a red
// test: it passes before this feature exists, because "no env at all" and "the
// branch contributed nothing" look identical from outside. It is here because it
// fails loudly against the two most likely wrong implementations.
//
// Declaring only one branch is legal, so a spec may carry YesEnv while the
// operator answers no. An implementation that assigns the chosen branch
// wholesale then hands the launcher an empty map, and the server comes up with
// its entire static environment wiped — because of a branch it did not take. One
// that writes keys into resolved[i].Env directly instead panics on a nil map.
func TestAskPrompts_UndeclaredBranchLeavesStaticEnvIntact(t *testing.T) {
	const key = "AATOOLKIT_PROMPT_ENV_PROBE"
	const keepVar = "AATOOLKIT_PROMPT_ENV_KEEP"

	input := []config.Server{{
		Name:    "svc",
		Type:    config.TypeExec,
		Command: "/bin/true",
		Env:     map[string]string{key: "static", keepVar: "keep-me"},
		Prompt: &config.PromptSpec{
			Question: "Use the local endpoint for this run?",
			YesEnv:   map[string]string{key: "chosen"},
			// NoEnv deliberately absent — the branch about to be taken.
		},
	}}

	var promptOut strings.Builder
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t)},
		bufio.NewReader(strings.NewReader("n\n")), &promptOut)

	resolved, err := eng.askPrompts(input)
	if err != nil {
		t.Fatalf("askPrompts: %v", err)
	}

	if got := resolved[0].Env[key]; got != "static" {
		t.Errorf("resolved Env[%s] = %q, want %q — an undeclared branch must contribute nothing", key, got, "static")
	}
	if got := resolved[0].Env[keepVar]; got != "keep-me" {
		t.Errorf("resolved Env[%s] = %q, want %q — taking an undeclared branch must not wipe the static env", keepVar, got, "keep-me")
	}
	if got := resolved[0].Env[key]; got == "chosen" {
		t.Errorf("resolved Env[%s] took the yes branch's value on a no answer", key)
	}
}

// TestAskPrompts_ResolvedEnvIsAlwaysTheCallersToOwn covers the review finding
// that mergedEnv's "never either input" contract had an exception: when the
// chosen branch declared no env it returned the caller's own map, so a caller
// writing to the resolved Env corrupted live config — in exactly the case a
// reader would not think to check. The branch taken here is the empty one.
func TestAskPrompts_ResolvedEnvIsAlwaysTheCallersToOwn(t *testing.T) {
	const key = "AATOOLKIT_PROMPT_ENV_PROBE"
	const sentinel = "AATOOLKIT_PROMPT_ENV_SENTINEL"

	input := []config.Server{{
		Name:    "svc",
		Type:    config.TypeExec,
		Command: "/bin/true",
		Env:     map[string]string{key: "static"},
		Prompt: &config.PromptSpec{
			Question: "Use the local endpoint for this run?",
			YesEnv:   map[string]string{key: "chosen"},
			// NoEnv absent, and the answer below is "n" — the empty branch.
		},
	}}
	inputEnv := input[0].Env

	var promptOut strings.Builder
	eng := NewEngine(config.Config{Supervisor: testSupervisor(t)},
		bufio.NewReader(strings.NewReader("n\n")), &promptOut)

	resolved, err := eng.askPrompts(input)
	if err != nil {
		t.Fatalf("askPrompts: %v", err)
	}

	resolved[0].Env[sentinel] = "x"
	if _, leaked := inputEnv[sentinel]; leaked {
		t.Errorf("writing to the resolved Env reached the caller's config — the returned map must always be the caller's to own, empty branch included")
	}
	if got := resolved[0].Env[key]; got != "static" {
		t.Errorf("resolved Env[%s] = %q, want %q", key, got, "static")
	}
}
