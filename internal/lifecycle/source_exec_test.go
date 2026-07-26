package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/config"
	"github.com/iansmith/aatoolkit/internal/health"
)

// sourceExecHealthServer builds a config.Server whose health check is the
// source-exec form (AATK-41): an exec probe at binPath, declared as built
// from pkgRelPath rather than assumed to already exist. Type is exec (a
// datastore whose own launch — e.g. "docker compose up" — is not something
// this fleet compiles), mirroring the ticket's own worked example; only the
// health probe is source-built.
func sourceExecHealthServer(binPath, pkgRelPath string) config.Server {
	return config.Server{
		Name: "datastore",
		Type: config.TypeExec,
		Health: config.Health{
			Exec: []string{binPath},
			Source: config.SourceExec{
				Build:  "go build -o " + binPath + " " + pkgRelPath,
				Binary: binPath,
			},
		},
	}
}

// ---- EnsureHealthProbeBuilt: DoD #5, unaffected when undeclared ----

func TestEnsureHealthProbeBuilt_UndeclaredSource_NoOp(t *testing.T) {
	s := config.Server{
		Name:   "svc",
		Type:   config.TypeExec,
		Health: config.Health{Exec: []string{"/bin/true"}},
	}

	result, err := EnsureHealthProbeBuilt(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != (BuildResult{}) {
		t.Fatalf("expected zero BuildResult when Health.Source isn't declared, got %+v", result)
	}
}

// ---- EnsureHealthProbeBuilt: DoD #1/#3, build only when stale ----

func TestEnsureHealthProbeBuilt_MissingArtifact_Builds(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "probe")

	s := sourceExecHealthServer(binPath, "./testdata/buildable_v1")

	result, err := EnsureHealthProbeBuilt(s)
	if err != nil {
		t.Fatalf("EnsureHealthProbeBuilt error: %v", err)
	}
	if !result.Replaced {
		t.Fatalf("expected Replaced=true for a missing artifact")
	}

	out, err := exec.Command(binPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running built probe: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "server-lifecycle-test-marker-v1") {
		t.Fatalf("expected v1 marker output, got %q", out)
	}
}

func TestEnsureHealthProbeBuilt_FreshArtifact_NotRebuilt(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "probe")
	seedOnDiskBinary(t, "./testdata/buildable_v1", binPath)

	before, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("ReadFile before: %v", err)
	}

	s := sourceExecHealthServer(binPath, "./testdata/buildable_v1")
	result, err := EnsureHealthProbeBuilt(s)
	if err != nil {
		t.Fatalf("EnsureHealthProbeBuilt error: %v", err)
	}
	if result.Replaced {
		t.Fatalf("expected Replaced=false — the on-disk artifact already matches a fresh build, so it must not be rebuilt")
	}

	after, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a not-stale artifact must not be modified")
	}
}

func TestEnsureHealthProbeBuilt_StaleArtifact_Rebuilds(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "probe")
	// Seed the on-disk artifact from v2's source; the declared build is v1's
	// — the two must differ, so the artifact is stale.
	seedOnDiskBinary(t, "./testdata/buildable_v2", binPath)

	s := sourceExecHealthServer(binPath, "./testdata/buildable_v1")
	result, err := EnsureHealthProbeBuilt(s)
	if err != nil {
		t.Fatalf("EnsureHealthProbeBuilt error: %v", err)
	}
	if !result.Replaced {
		t.Fatalf("expected Replaced=true — on-disk artifact was built from different source")
	}

	out, err := exec.Command(binPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running rebuilt probe: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "server-lifecycle-test-marker-v1") {
		t.Fatalf("expected rebuilt artifact to run v1's marker, got %q", out)
	}
}

// ---- EnsureHealthProbeBuilt: DoD #4, build failure distinct from unhealthy ----

func TestEnsureHealthProbeBuilt_BuildFailure_SurfacesCompilerOutput(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "probe")

	s := sourceExecHealthServer(binPath, "./testdata/unbuildable_probe")

	_, err := EnsureHealthProbeBuilt(s)
	if err == nil {
		t.Fatalf("expected error when the probe's source fails to compile")
	}
	// Distinguishable from an unhealthy probe: the error names the build
	// step, not a probe exit status.
	if !strings.Contains(err.Error(), "building health probe") {
		t.Fatalf("expected error to identify this as a build failure, got: %v", err)
	}
	// Carries the compiler's own diagnostic, not a bare status.
	if !strings.Contains(err.Error(), "undefinedProbeFixtureSymbol") {
		t.Fatalf("expected error to carry the compiler's output, got: %v", err)
	}
}

// ---- EnsureHealthProbeBuilt: DoD #2, build-before-probe ordering ----

// TestEnsureHealthProbeBuilt_ProbeSucceedsOnlyAfterBuild establishes that the
// build is a genuine precondition of a passing probe: probing the declared
// exec spec BEFORE calling EnsureHealthProbeBuilt must fail (the artifact does
// not exist yet), and probing again AFTER must succeed. A probe that could
// pass beforehand would mean the build was never load-bearing at all — the
// property the 2s-health-timeout-vs-cold-build gap
// (design/aa-server-status.md §6.1) depends on getting right.
//
// It does NOT prove the production call order. This test drives the two steps
// itself, so it stays green even if buildProbeAndPollReady is reversed. The
// test that pins the real wiring is
// TestRealEngine_Up_BuildsHealthProbeFromSourceBeforeReachingHealthy in
// cmd/aa-server-status — verified by reversing that call order and watching it,
// and not this one, go red. Do not delete that test believing this one covers
// the ordering.
func TestEnsureHealthProbeBuilt_ProbeSucceedsOnlyAfterBuild(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "probe")

	s := sourceExecHealthServer(binPath, "./testdata/buildable_v1")
	spec := health.ResolveSpec(s.Health, "", 0)

	before := health.Probe(context.Background(), spec, time.Second)
	if before.Healthy {
		t.Fatalf("expected the probe to fail before the artifact is built, got healthy")
	}

	if _, err := EnsureHealthProbeBuilt(s); err != nil {
		t.Fatalf("EnsureHealthProbeBuilt error: %v", err)
	}

	after := health.Probe(context.Background(), spec, time.Second)
	if !after.Healthy {
		t.Fatalf("expected the probe to succeed once the build has run, got unhealthy: %+v", after)
	}
}
