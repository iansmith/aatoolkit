package main

import "testing"

// These tests describe the expected --config flag behavior.
//
// They once covered localConfigPath alongside parseFlags — the helper that
// derived a sibling ".local.toml" from the config path. AATK-33 deleted the
// overlay, so that helper and its four tests went with it.

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
