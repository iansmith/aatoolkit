package config

import (
	"strings"
	"testing"
)

// --- decoding the source-exec form (AATK-41) ---

func TestHealth_UnmarshalTOML_SourceExecForm(t *testing.T) {
	h := decodeHealth(t, `health = { exec = ["build/db-probe"], source = { build = "go build -o build/db-probe ./cmd/db-probe", binary = "build/db-probe" } }`)
	want := SourceExec{Build: "go build -o build/db-probe ./cmd/db-probe", Binary: "build/db-probe"}
	if h.Source != want {
		t.Errorf("Source = %+v, want %+v", h.Source, want)
	}
	if !h.Source.Declared() {
		t.Errorf("Declared() = false, want true for a fully-populated SourceExec")
	}
}

func TestHealth_UnmarshalTOML_NoSource_UndeclaredByDefault(t *testing.T) {
	// DoD #5: a Health decoded before this field existed carries a zero
	// SourceExec, and Declared() must say so.
	h := decodeHealth(t, `health = { exec = ["db-probe"] }`)
	if h.Source.Declared() {
		t.Errorf("Declared() = true for a health form with no source table, want false")
	}
}

func TestHealth_UnmarshalTOML_SourceForm_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"not a table", `health = { exec = ["db-probe"], source = "go build -o db-probe ./cmd" }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := decodeHealthErr(t, tc.src); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// --- SourceExec.Declared ---

func TestSourceExec_Declared(t *testing.T) {
	for _, tc := range []struct {
		name string
		se   SourceExec
		want bool
	}{
		{"zero value", SourceExec{}, false},
		{"build only", SourceExec{Build: "go build -o x ./cmd/x"}, true},
		{"binary only", SourceExec{Binary: "x"}, true},
		{"both", SourceExec{Build: "go build -o x ./cmd/x", Binary: "x"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.se.Declared(); got != tc.want {
				t.Errorf("Declared() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- validation ---

// sourceExecServer is execServer's counterpart declaring a source-exec probe:
// a datastore whose exit-0 probe program is built from source rather than
// assumed to already exist (AATK-41 — the gap AATK-36's exec form left
// behind).
func sourceExecServer(overrides func(*Server)) Server {
	return execServer(func(s *Server) {
		s.Health = Health{
			Exec: []string{"build/db-probe"},
			Source: SourceExec{
				Build:  "go build -o build/db-probe ./cmd/db-probe",
				Binary: "build/db-probe",
			},
		}
		if overrides != nil {
			overrides(s)
		}
	})
}

func TestValidate_HealthSourceExecFormPasses(t *testing.T) {
	if err := Validate(Config{Servers: []Server{sourceExecServer(nil)}}); err != nil {
		t.Fatalf("expected source-exec health to validate, got %v", err)
	}
}

// The same thing through the path an operator actually takes — strict decode
// plus Validate, from a real file — mirroring TestLoad_HealthExecFormLoadsAndValidates.
func TestLoad_HealthSourceExecFormLoadsAndValidates(t *testing.T) {
	cfg, err := Load("testdata/health-source-exec.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(cfg.Servers))
	}

	datastore := cfg.Servers[0]
	want := SourceExec{Build: "go build -o build/db-probe ./cmd/db-probe", Binary: "build/db-probe"}
	if datastore.Health.Source != want {
		t.Errorf("Health.Source decoded as %+v, want %+v", datastore.Health.Source, want)
	}
}

// Declaring health.source missing either half must name which — mirroring
// how the rest of this file names the missing half of a required pair.
func TestValidate_HealthSource_MissingHalf_Rejected(t *testing.T) {
	for _, tc := range []struct {
		name       string
		overrides  func(*Server)
		mustName   string
		mustNotErr string
	}{
		{
			name: "binary without build",
			overrides: func(s *Server) {
				s.Health.Source = SourceExec{Binary: "build/db-probe"}
			},
			mustName: "build",
		},
		{
			name: "build without binary",
			overrides: func(s *Server) {
				s.Health.Source = SourceExec{Build: "go build -o build/db-probe ./cmd/db-probe"}
			},
			mustName: "binary",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sourceExecServer(tc.overrides)
			err := Validate(Config{Servers: []Server{s}})
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.mustName) {
				t.Errorf("error %q must name %q", err.Error(), tc.mustName)
			}
		})
	}
}

// Declaring health.source alongside the HTTP form (a "conflicting existing
// field", per the ticket's test expectations) is rejected: there is no
// program to build for a GET.
func TestValidate_HealthSource_WithoutExec_Rejected(t *testing.T) {
	s := validServer(func(s *Server) {
		s.Health = Health{
			Path:   "/healthz",
			Source: SourceExec{Build: "go build -o build/db-probe ./cmd/db-probe", Binary: "build/db-probe"},
		}
	})
	err := Validate(Config{Servers: []Server{s}})
	if err == nil {
		t.Fatalf("expected error declaring health.source with the HTTP form, got nil")
	}
	for _, want := range []string{"source", "exec"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %q", err.Error(), want)
		}
	}
}

// health.exec[0] must name the program health.source builds — a mismatch
// would build one path and probe another.
func TestValidate_HealthSource_ExecArgv0MismatchesBinary_Rejected(t *testing.T) {
	s := sourceExecServer(func(s *Server) {
		s.Health.Exec = []string{"build/some-other-binary"}
	})
	err := Validate(Config{Servers: []Server{s}})
	if err == nil {
		t.Fatalf("expected error when health.exec[0] does not match health.source.binary, got nil")
	}
	if !strings.Contains(err.Error(), "build/some-other-binary") || !strings.Contains(err.Error(), "build/db-probe") {
		t.Errorf("error %q must name both the exec[0] value and health.source.binary", err.Error())
	}
}

// DoD #5: a server declaring no source-exec form validates exactly as it did
// before this field existed — execServer's own pre-existing coverage
// (TestValidate_HealthExecFormPasses) already pins this; this test just
// makes the "unaffected" claim explicit for the zero-value SourceExec case.
func TestValidate_HealthSource_Undeclared_ServerUnaffected(t *testing.T) {
	s := execServer(nil)
	if s.Health.Source.Declared() {
		t.Fatalf("precondition: execServer must not declare health.source")
	}
	if err := Validate(Config{Servers: []Server{s}}); err != nil {
		t.Fatalf("expected a server with no health.source to validate exactly as before, got %v", err)
	}
}
