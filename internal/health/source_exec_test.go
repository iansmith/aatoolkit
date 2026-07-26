package health

import (
	"slices"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// TestResolveSpec_SourceDeclared_ArgvUnaffected pins the ticket's test
// expectation directly: a source-declared probe (AATK-41) resolves to the
// exact same argv shape probeExec already runs for a plain exec health
// check — config.Health.Source is purely a build-time declaration consumed
// by internal/lifecycle.EnsureHealthProbeBuilt before the first probe;
// nothing about how the probe itself is executed changes, so AATK-36's
// execution path (probeExec) is reused rather than duplicated.
func TestResolveSpec_SourceDeclared_ArgvUnaffected(t *testing.T) {
	h := config.Health{
		Exec: []string{"build/db-probe", "--port", "5432"},
		Source: config.SourceExec{
			Build:  "go build -o build/db-probe ./cmd/db-probe",
			Binary: "build/db-probe",
		},
	}

	spec := ResolveSpec(h, "127.0.0.1", 9000)

	want := []string{"build/db-probe", "--port", "5432"}
	if !slices.Equal(spec.Exec, want) {
		t.Errorf("Exec = %q, want %q", spec.Exec, want)
	}
	if !spec.IsExec() {
		t.Errorf("IsExec() = false, want true")
	}
}
