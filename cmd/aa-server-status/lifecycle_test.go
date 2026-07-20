package main

import (
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// --- edge / boundary cases ---

func TestStubEngine_StatusEmptyConfig(t *testing.T) {
	eng := NewStubEngine(config.Config{})
	statuses := eng.Status()
	if len(statuses) != 0 {
		t.Fatalf("expected no statuses for empty config, got %+v", statuses)
	}
}

func TestStubEngine_StatusIncludesDisabledServers(t *testing.T) {
	cfg := config.Config{Servers: []config.Server{
		{Name: "enabled-one", Enabled: true},
		{Name: "disabled-one", Enabled: false},
	}}
	eng := NewStubEngine(cfg)
	statuses := eng.Status()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses (enabled + disabled), got %d: %+v", len(statuses), statuses)
	}
	var sawDisabled bool
	for _, s := range statuses {
		if s.Name == "disabled-one" {
			sawDisabled = true
			if s.Enabled {
				t.Fatalf("disabled-one should report Enabled=false, got %+v", s)
			}
		}
	}
	if !sawDisabled {
		t.Fatalf("disabled server missing from status list: %+v", statuses)
	}
}

// --- error / rejection cases ---

func TestStubEngine_UpIsNotImplementedLoudly(t *testing.T) {
	eng := NewStubEngine(config.Config{Servers: []config.Server{{Name: "s1", Enabled: true}}})
	err := eng.Up("s1")
	if err == nil {
		t.Fatal("stub engine Up() should return a loud not-implemented error, not nil")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("expected 'not implemented' in error, got: %v", err)
	}
}

func TestStubEngine_DownIsNotImplementedLoudly(t *testing.T) {
	eng := NewStubEngine(config.Config{})
	if err := eng.Down("s1"); err == nil {
		t.Fatal("stub engine Down() should return a loud not-implemented error, not nil")
	}
}

func TestStubEngine_DeadIsNotImplementedLoudly(t *testing.T) {
	eng := NewStubEngine(config.Config{})
	if err := eng.Dead("s1"); err == nil {
		t.Fatal("stub engine Dead() should return a loud not-implemented error, not nil")
	}
}

func TestStubEngine_BuildIsNotImplementedLoudly(t *testing.T) {
	eng := NewStubEngine(config.Config{})
	if err := eng.Build("s1"); err == nil {
		t.Fatal("stub engine Build() should return a loud not-implemented error, not nil")
	}
}

func TestStubEngine_LogsIsNotImplementedLoudly(t *testing.T) {
	eng := NewStubEngine(config.Config{})
	if _, err := eng.Logs("s1"); err == nil {
		t.Fatal("stub engine Logs() should return a loud not-implemented error, not nil")
	}
}

// --- cross-feature interaction ---

func TestStubEngine_TeardownAllReturnsAllOwnedRegardlessOfEnabled(t *testing.T) {
	cfg := config.Config{Servers: []config.Server{
		{Name: "enabled-one", Enabled: true},
		{Name: "disabled-one", Enabled: false},
	}}
	eng := NewStubEngine(cfg)
	owned := eng.TeardownAll()
	if len(owned) != 2 {
		t.Fatalf("TeardownAll should tear down every owned child (enabled or not), got %+v", owned)
	}
	names := map[string]bool{}
	for _, n := range owned {
		names[n] = true
	}
	if !names["enabled-one"] || !names["disabled-one"] {
		t.Fatalf("TeardownAll must include disabled children too, got %+v", owned)
	}
}

// --- happy path ---

func TestStubEngine_StatusReflectsConfig(t *testing.T) {
	cfg := config.Config{Servers: []config.Server{{Name: "server", Enabled: true}}}
	eng := NewStubEngine(cfg)
	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].Name != "server" {
		t.Fatalf("expected one status for 'server', got %+v", statuses)
	}
}

// --- SOP-19: status table rendering columns ---

func TestStubEngine_StatusIncludesConfiguredType(t *testing.T) {
	// TYPE is the one new column the stub can already populate for free —
	// it comes straight from the static config, no observation needed.
	cfg := config.Config{Servers: []config.Server{{Name: "server", Type: config.TypeMLX, Enabled: true}}}
	eng := NewStubEngine(cfg)
	statuses := eng.Status()
	if len(statuses) != 1 || statuses[0].Type != config.TypeMLX {
		t.Fatalf("expected Type to reflect the configured server type, got %+v", statuses)
	}
}

func TestStubEngine_StatusPlaceholdersForUnwiredColumns(t *testing.T) {
	// Real observation (PID, ports, health, anomalies) is a downstream
	// reconciliation ticket — until it lands, the stub must supply
	// harmless zero-valued placeholders rather than fabricating data.
	cfg := config.Config{Servers: []config.Server{{Name: "server", Enabled: true}}}
	eng := NewStubEngine(cfg)
	s := eng.Status()[0]
	if s.PID != 0 {
		t.Fatalf("expected placeholder PID 0 (not yet wired), got %d", s.PID)
	}
	if s.Health != "" {
		t.Fatalf("expected placeholder empty Health (not yet wired), got %q", s.Health)
	}
	if len(s.Ports) != 0 {
		t.Fatalf("expected placeholder empty Ports (not yet wired), got %+v", s.Ports)
	}
	if s.OwnedDisabled || s.Stale {
		t.Fatalf("expected no anomaly flags set by the stub, got %+v", s)
	}
}
