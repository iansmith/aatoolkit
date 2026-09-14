package main

import "testing"

// These tests describe the expected --config flag behavior.
//
// They once covered localConfigPath alongside parseFlags — the helper that
// derived a sibling ".local.toml" from the config path. AATK-33 deleted the
// overlay, so that helper and its four tests went with it.

// --- parseFlags: default, happy path, and error/rejection cases ---

func TestParseFlags_DefaultsToBasePathWhenNoFlag(t *testing.T) {
	configPath, autoMode, err := parseFlags([]string{})
	if err != nil {
		t.Fatalf("parseFlags([]) unexpected error: %v", err)
	}
	if configPath != defaultBasePath {
		t.Errorf("configPath = %q, want %q", configPath, defaultBasePath)
	}
	if autoMode != "" {
		t.Errorf("autoMode = %q, want empty", autoMode)
	}
}

func TestParseFlags_ConfigFlagOverridesDefault(t *testing.T) {
	configPath, _, err := parseFlags([]string{"--config", "alt-fleet.toml"})
	if err != nil {
		t.Fatalf("parseFlags(--config alt-fleet.toml) unexpected error: %v", err)
	}
	if configPath != "alt-fleet.toml" {
		t.Errorf("configPath = %q, want %q", configPath, "alt-fleet.toml")
	}
}

func TestParseFlags_EqualsFormAccepted(t *testing.T) {
	configPath, _, err := parseFlags([]string{"--config=alt-fleet.toml"})
	if err != nil {
		t.Fatalf("parseFlags(--config=alt-fleet.toml) unexpected error: %v", err)
	}
	if configPath != "alt-fleet.toml" {
		t.Errorf("configPath = %q, want %q", configPath, "alt-fleet.toml")
	}
}

func TestParseFlags_MissingValueReturnsError(t *testing.T) {
	_, _, err := parseFlags([]string{"--config"})
	if err == nil {
		t.Error("parseFlags(--config) with no value: expected error, got nil")
	}
}

func TestParseFlags_UnknownFlagReturnsError(t *testing.T) {
	_, _, err := parseFlags([]string{"--bogus"})
	if err == nil {
		t.Error("parseFlags(--bogus) with unknown flag: expected error, got nil")
	}
}

// --- parseFlags: --auto flag ---

func TestParseFlags_AutoUpReturnsAutoMode(t *testing.T) {
	_, autoMode, err := parseFlags([]string{"--auto", "up"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if autoMode != "up" {
		t.Errorf("autoMode = %q, want %q", autoMode, "up")
	}
}

func TestParseFlags_AutoDownReturnsAutoMode(t *testing.T) {
	_, autoMode, err := parseFlags([]string{"--auto", "down"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if autoMode != "down" {
		t.Errorf("autoMode = %q, want %q", autoMode, "down")
	}
}

func TestParseFlags_AutoWithConfigBothWork(t *testing.T) {
	configPath, autoMode, err := parseFlags([]string{"--config", "fleet.toml", "--auto", "up"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if configPath != "fleet.toml" {
		t.Errorf("configPath = %q, want %q", configPath, "fleet.toml")
	}
	if autoMode != "up" {
		t.Errorf("autoMode = %q, want %q", autoMode, "up")
	}
}

func TestParseFlags_AutoInvalidModeReturnsError(t *testing.T) {
	_, _, err := parseFlags([]string{"--auto", "bogus"})
	if err == nil {
		t.Error("expected error for --auto bogus, got nil")
	}
}
