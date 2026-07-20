package main

import "testing"

// These tests describe the expected --config flag behavior for SOP-32.
// They fail against the Phase 0 stubs in main.go (parseFlags/localConfigPath
// currently ignore their inputs).

// --- localConfigPath: derivation convention (edge/boundary cases first) ---

func TestLocalConfigPath_SwapsTomlSuffix(t *testing.T) {
	got := localConfigPath("custom.toml")
	want := "custom.local.toml"
	if got != want {
		t.Errorf("localConfigPath(%q) = %q, want %q", "custom.toml", got, want)
	}
}

func TestLocalConfigPath_AppendsWhenNoTomlSuffix(t *testing.T) {
	got := localConfigPath("customconfig")
	want := "customconfig.local.toml"
	if got != want {
		t.Errorf("localConfigPath(%q) = %q, want %q", "customconfig", got, want)
	}
}

func TestLocalConfigPath_DefaultMatchesExistingConvention(t *testing.T) {
	// Cross-feature interaction: the derived local path for the default
	// base path must equal the pre-existing defaultLocalPath constant, so
	// omitting --config keeps today's behavior unchanged.
	got := localConfigPath(defaultBasePath)
	if got != defaultLocalPath {
		t.Errorf("localConfigPath(%q) = %q, want %q (defaultLocalPath)", defaultBasePath, got, defaultLocalPath)
	}
}

func TestLocalConfigPath_NestedPathSwapsOnlyFinalSuffix(t *testing.T) {
	got := localConfigPath("configs/fleet.toml")
	want := "configs/fleet.local.toml"
	if got != want {
		t.Errorf("localConfigPath(%q) = %q, want %q", "configs/fleet.toml", got, want)
	}
}

// --- parseFlags: default, happy path, and error/rejection cases ---

func TestParseFlags_DefaultsToBasePathWhenNoFlag(t *testing.T) {
	got, err := parseFlags([]string{})
	if err != nil {
		t.Fatalf("parseFlags([]) unexpected error: %v", err)
	}
	if got != defaultBasePath {
		t.Errorf("parseFlags([]) = %q, want %q", got, defaultBasePath)
	}
}

func TestParseFlags_ConfigFlagOverridesDefault(t *testing.T) {
	got, err := parseFlags([]string{"--config", "alt-fleet.toml"})
	if err != nil {
		t.Fatalf("parseFlags(--config alt-fleet.toml) unexpected error: %v", err)
	}
	if got != "alt-fleet.toml" {
		t.Errorf("parseFlags(--config alt-fleet.toml) = %q, want %q", got, "alt-fleet.toml")
	}
}

func TestParseFlags_EqualsFormAccepted(t *testing.T) {
	got, err := parseFlags([]string{"--config=alt-fleet.toml"})
	if err != nil {
		t.Fatalf("parseFlags(--config=alt-fleet.toml) unexpected error: %v", err)
	}
	if got != "alt-fleet.toml" {
		t.Errorf("parseFlags(--config=alt-fleet.toml) = %q, want %q", got, "alt-fleet.toml")
	}
}

func TestParseFlags_MissingValueReturnsError(t *testing.T) {
	_, err := parseFlags([]string{"--config"})
	if err == nil {
		t.Error("parseFlags(--config) with no value: expected error, got nil")
	}
}

func TestParseFlags_UnknownFlagReturnsError(t *testing.T) {
	_, err := parseFlags([]string{"--bogus"})
	if err == nil {
		t.Error("parseFlags(--bogus) with unknown flag: expected error, got nil")
	}
}
