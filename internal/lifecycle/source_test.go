package lifecycle

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// buildFixture returns a config.Server whose Build command compiles the
// testdata fixture package at pkgRelPath (relative to this package's source
// directory, e.g. "./testdata/buildable_v1") to destPath. destPath is used
// as both the "on-disk binary" path and the source of the -o token being
// rewritten by the code under test.
func buildFixture(name, pkgRelPath, destPath string) config.Server {
	return config.Server{
		Name:   name,
		Type:   config.TypeSource,
		Build:  "go build -o " + destPath + " " + pkgRelPath,
		Binary: destPath,
	}
}

// seedOnDiskBinary builds pkgRelPath (with -buildvcs=false, matching what
// ProbeStaleness itself would run) straight to destPath, simulating "this
// is what was last deployed."
func seedOnDiskBinary(t *testing.T, pkgRelPath, destPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cmd := exec.Command("go", "build", "-o", destPath, "-buildvcs=false", pkgRelPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding on-disk binary: %v\n%s", err, out)
	}
}

// ---- SourceCommand / LaunchSource ----

func TestSourceCommand_VerbatimBinaryAndArgs(t *testing.T) {
	s := config.Server{
		Name:   "server",
		Type:   config.TypeSource,
		Binary: "build/server",
		Args:   []string{"--flag-from-config"},
		Host:   "127.0.0.1", // must be ignored — source launcher does not auto-append host/port
		Port:   0,
	}

	command, args := SourceCommand(s)

	if command != "build/server" {
		t.Fatalf("expected command 'build/server', got %q", command)
	}
	want := []string{"--flag-from-config"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected verbatim args %v, got %v", want, args)
	}
}

func TestSourceCommand_NoAutoAppendedHostPortFlags(t *testing.T) {
	s := config.Server{
		Name:    "server",
		Type:    config.TypeSource,
		Binary:  "build/server",
		Args:    []string{"serve"},
		Host:    "127.0.0.1",
		Port:    9730,
		Listens: []int{9730},
	}

	_, args := SourceCommand(s)

	for _, a := range args {
		if a == "--host" || a == "--port" || a == "127.0.0.1" || a == "9730" {
			t.Fatalf("source launcher must not auto-append host/port flags, got args %v", args)
		}
	}
}

func TestSourceCommand_EmptyArgsAllowed(t *testing.T) {
	s := config.Server{
		Name:   "minimal",
		Type:   config.TypeSource,
		Binary: "build/minimal",
	}

	command, args := SourceCommand(s)

	if command != "build/minimal" {
		t.Fatalf("expected command 'build/minimal', got %q", command)
	}
	if len(args) != 0 {
		t.Fatalf("expected no args, got %v", args)
	}
}

func TestLaunchSource_RunsConfiguredBinaryAndArgs(t *testing.T) {
	dir := t.TempDir()

	s := config.Server{
		Name:   "echoer",
		Type:   config.TypeSource,
		Binary: "/bin/sh",
		Args:   []string{"-c", "echo source-launch-marker"},
	}

	proc, err := LaunchSource(dir, s)
	if err != nil {
		t.Fatalf("LaunchSource error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	content := waitForLogContent(t, proc.LogPath, "source-launch-marker")
	if !strings.Contains(content, "source-launch-marker") {
		t.Fatalf("expected launched binary's output in log, got: %q", content)
	}
}

// TestLaunchSource_RelativeBinaryResolvesAgainstOwnCwdNotDir guards against a
// regression: LaunchSource now threads s.Dir into cmd.Dir (per
// design/aa-server-status.md §7), but a relative s.Binary must still resolve
// against aa-server-status's own launch cwd — where the build machinery in
// this file actually writes it — not against s.Dir (which here exists only
// to tell `go build` where to find source, per s.Dir's pre-existing role).
func TestLaunchSource_RelativeBinaryResolvesAgainstOwnCwdNotDir(t *testing.T) {
	ownCwd := t.TempDir()
	otherDir := t.TempDir() // s.Dir — must NOT affect Binary resolution

	scriptRel := "myserver.sh"
	scriptAbs := filepath.Join(ownCwd, scriptRel)
	if err := os.WriteFile(scriptAbs, []byte("#!/bin/sh\necho relative-binary-marker\n"), 0o755); err != nil {
		t.Fatalf("writing script: %v", err)
	}

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(ownCwd); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer os.Chdir(origWd)

	s := config.Server{
		Name:   "relbin",
		Type:   config.TypeSource,
		Binary: scriptRel,
		Dir:    otherDir,
	}

	logDir := t.TempDir()
	proc, err := LaunchSource(logDir, s)
	if err != nil {
		t.Fatalf("LaunchSource error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	content := waitForLogContent(t, proc.LogPath, "relative-binary-marker")
	if !strings.Contains(content, "relative-binary-marker") {
		t.Fatalf("expected relative Binary %q to launch from own cwd %q despite Dir=%q set; got log: %q", scriptRel, ownCwd, otherDir, content)
	}
}

// ---- RewriteBuildOutput ----

func TestRewriteBuildOutput_ReplacesPathToken(t *testing.T) {
	got, err := RewriteBuildOutput("go build -o build/server ./cmd/server", "/tmp/xyz/server")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "go build -o /tmp/xyz/server ./cmd/server"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRewriteBuildOutput_MissingDashO_Errors(t *testing.T) {
	_, err := RewriteBuildOutput("go build ./cmd/server", "/tmp/xyz/server")
	if err == nil {
		t.Fatalf("expected error for build command missing -o, got nil")
	}
	if !strings.Contains(err.Error(), "-o") {
		t.Fatalf("expected error to mention '-o', got: %v", err)
	}
}

func TestRewriteBuildOutput_DashOAsLastToken_Errors(t *testing.T) {
	_, err := RewriteBuildOutput("go build ./cmd/server -o", "/tmp/xyz/server")
	if err == nil {
		t.Fatalf("expected error when -o has no following path token, got nil")
	}
}

func TestRewriteBuildOutput_EqualsForm_NotTreatedAsDashO(t *testing.T) {
	// "-o=path" is a single token, not the "-o <path>" pair the ticket
	// specifies — must NOT be silently accepted; must hard-error like any
	// other missing -o case, not partially rewrite it.
	_, err := RewriteBuildOutput("go build -o=build/server ./cmd/server", "/tmp/xyz/server")
	if err == nil {
		t.Fatalf("expected error for -o=path (not a token pair), got nil")
	}
}

func TestRewriteBuildOutput_LongFlagForm_NotFalselyMatched(t *testing.T) {
	// "--output" contains "-o" only as a substring — must not be matched by
	// a naive substring search.
	_, err := RewriteBuildOutput("go build --output build/server ./cmd/server", "/tmp/xyz/server")
	if err == nil {
		t.Fatalf("expected error — '--output' must not be mistaken for '-o'")
	}
}

func TestRewriteBuildOutput_EmptyBuildCommand_Errors(t *testing.T) {
	_, err := RewriteBuildOutput("", "/tmp/xyz/server")
	if err == nil {
		t.Fatalf("expected error for empty build command, got nil")
	}
}

// ---- expandTilde / insertGoDirFlag ----

func TestExpandTilde_LeadingTildeSlash(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	got, err := expandTilde("~/slopstop/router")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, "slopstop/router")
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestExpandTilde_BareTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	got, err := expandTilde("~")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != home {
		t.Fatalf("expected %q, got %q", home, got)
	}
}

func TestExpandTilde_NoTilde_Passthrough(t *testing.T) {
	got, err := expandTilde("/already/absolute")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/already/absolute" {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func TestExpandTilde_TildeNotAtStart_NotExpanded(t *testing.T) {
	// "a~/b" is not a home-relative path — must not be rewritten.
	got, err := expandTilde("a~/b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "a~/b" {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func TestInsertGoDirFlag_InsertsRightAfterGo(t *testing.T) {
	got, err := insertGoDirFlag([]string{"go", "build", "-o", "/tmp/x", "."}, "/some/dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"go", "-C", "/some/dir", "build", "-o", "/tmp/x", "."}
	if !slices.Equal(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestInsertGoDirFlag_ExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	got, err := insertGoDirFlag([]string{"go", "build", "-o", "/tmp/x", "."}, "~/slopstop/router")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, "slopstop/router")
	if got[2] != want {
		t.Fatalf("expected expanded dir %q at index 2, got %v", want, got)
	}
}

func TestInsertGoDirFlag_NonGoCommand_Errors(t *testing.T) {
	if _, err := insertGoDirFlag([]string{"make", "build"}, "/some/dir"); err == nil {
		t.Fatal("expected error for non-'go' build command, got nil")
	}
}

func TestInsertGoDirFlag_EmptyFields_Errors(t *testing.T) {
	if _, err := insertGoDirFlag(nil, "/some/dir"); err == nil {
		t.Fatal("expected error for empty fields, got nil")
	}
}

// ---- ProbeStaleness ----

func TestProbeStaleness_IdenticalBinary_NotStale(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v1", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	result, err := ProbeStaleness(s)
	if err != nil {
		t.Fatalf("ProbeStaleness error: %v", err)
	}
	defer result.Cleanup()

	if result.Stale {
		t.Fatalf("expected not stale — same source, same build flags")
	}
}

func TestProbeStaleness_DifferentBinary_Stale(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	// Seed the "on-disk" binary from v2's source, then probe staleness
	// against v1's build command — the two must differ.
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	result, err := ProbeStaleness(s)
	if err != nil {
		t.Fatalf("ProbeStaleness error: %v", err)
	}
	defer result.Cleanup()

	if !result.Stale {
		t.Fatalf("expected stale — on-disk binary was built from different source")
	}
}

func TestProbeStaleness_NeverTouchesOnDiskBinary(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	before, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("ReadFile before probe: %v", err)
	}

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)
	result, err := ProbeStaleness(s)
	if err != nil {
		t.Fatalf("ProbeStaleness error: %v", err)
	}
	defer result.Cleanup()

	after, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("ReadFile after probe: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("staleness probe must never modify the on-disk binary, but its contents changed")
	}
}

func TestProbeStaleness_MissingOnDiskBinary_Stale(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "does-not-exist-yet")

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	result, err := ProbeStaleness(s)
	if err != nil {
		t.Fatalf("ProbeStaleness error: %v", err)
	}
	defer result.Cleanup()

	if !result.Stale {
		t.Fatalf("expected stale when on-disk binary does not exist at all")
	}
}

func TestProbeStaleness_BuildCommandMissingDashO_Errors(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")

	s := config.Server{
		Name:   "server",
		Type:   config.TypeSource,
		Build:  "go build ./testdata/buildable_v1", // no -o
		Binary: onDisk,
	}

	_, err := ProbeStaleness(s)
	if err == nil {
		t.Fatalf("expected error for build command missing -o, got nil")
	}
}

func TestProbeStaleness_NonSourceType_Errors(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "chat-llm")

	s := config.Server{
		Name:   "chat-llm",
		Type:   config.TypeMLX,
		Build:  "go build -o " + onDisk + " ./testdata/buildable_v1",
		Binary: onDisk,
	}

	_, err := ProbeStaleness(s)
	if err == nil {
		t.Fatalf("expected loud error probing staleness of a non-source server, got nil")
	}
	if !strings.Contains(err.Error(), "chat-llm") {
		t.Fatalf("expected error to name the server, got: %v", err)
	}
}

func TestProbeStaleness_BuildFailure_SurfacesOutput(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")

	s := buildFixture("server", "./testdata/no-such-package-xyz", onDisk)

	_, err := ProbeStaleness(s)
	if err == nil {
		t.Fatalf("expected error when the build command's package path doesn't exist")
	}
}

func TestProbeStaleness_DirBuildsFromExternalPackage(t *testing.T) {
	// Dir set, Build's package pattern is "." — proves -C actually changes
	// where go looks for source (buildable_v1's package, not this test's own
	// directory), while the -o output still lands wherever the caller
	// requested (already rewritten to a temp path by rewriteBuildOutputFields,
	// same as an in-repo source server).
	fixtureDir, err := filepath.Abs("./testdata/buildable_v1")
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}

	dir := t.TempDir()
	onDisk := filepath.Join(dir, "external") // doesn't exist yet — first build

	s := config.Server{
		Name:   "external",
		Type:   config.TypeSource,
		Build:  "go build -o build/external .",
		Binary: onDisk,
		Dir:    fixtureDir,
	}

	result, err := ProbeStaleness(s)
	if err != nil {
		t.Fatalf("ProbeStaleness error: %v", err)
	}
	defer result.Cleanup()

	if !result.Stale {
		t.Fatal("expected stale — no on-disk binary yet")
	}

	out, err := exec.Command(result.TempBinary).CombinedOutput()
	if err != nil {
		t.Fatalf("running built binary: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "server-lifecycle-test-marker-v1") {
		t.Fatalf("expected buildable_v1's marker output, got %q — build did not source from Dir", out)
	}
}

func TestProbeStaleness_DirWithNonGoBuildCommand_Errors(t *testing.T) {
	s := config.Server{
		Name:   "external",
		Type:   config.TypeSource,
		Build:  "make -o build/external",
		Binary: "build/external",
		Dir:    "/some/external/project",
	}

	_, err := ProbeStaleness(s)
	if err == nil {
		t.Fatal("expected error when 'Dir' is set on a non-'go' build command")
	}
}

func TestProbeStaleness_DashVCSFalse_AppliedRegardlessOfPositionInBuildString(t *testing.T) {
	// The build string's package pattern comes after -o in every real
	// config example; confirm -buildvcs=false is inserted such that `go
	// build` still parses successfully (it must land before the package
	// pattern, not appended at the very end where it would be
	// misinterpreted as a package pattern once flag parsing has stopped).
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	result, err := ProbeStaleness(s)
	if err != nil {
		t.Fatalf("ProbeStaleness error (likely -buildvcs=false misplacement): %v", err)
	}
	defer result.Cleanup()
}

// ---- PerformBuild ----

func TestPerformBuild_IdenticalBinary_NoOp(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v1", onDisk)

	before, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("ReadFile before: %v", err)
	}

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	result, err := PerformBuild(s, nil)
	if err != nil {
		t.Fatalf("PerformBuild error: %v", err)
	}
	if result.Replaced {
		t.Fatalf("expected no-op (Replaced=false) when temp build is identical to on-disk binary")
	}

	after, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("no-op build must not modify the on-disk binary")
	}
}

func TestPerformBuild_DifferentBinary_ReplacesOnDisk(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	result, err := PerformBuild(s, nil)
	if err != nil {
		t.Fatalf("PerformBuild error: %v", err)
	}
	if !result.Replaced {
		t.Fatalf("expected Replaced=true when on-disk binary differs from fresh build")
	}

	// After replace, on-disk binary must now match a fresh build of v1's
	// source.
	freshTemp := filepath.Join(dir, "fresh-v1")
	cmd := exec.Command("go", "build", "-o", freshTemp, "-buildvcs=false", "./testdata/buildable_v1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fresh comparison binary: %v\n%s", err, out)
	}

	onDiskContent, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("ReadFile on-disk after replace: %v", err)
	}
	freshContent, err := os.ReadFile(freshTemp)
	if err != nil {
		t.Fatalf("ReadFile fresh comparison binary: %v", err)
	}
	if !reflect.DeepEqual(onDiskContent, freshContent) {
		t.Fatalf("expected on-disk binary to match a fresh v1 build after replace")
	}
}

func TestPerformBuild_ReplacedBinary_IsExecutable(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	if _, err := PerformBuild(s, nil); err != nil {
		t.Fatalf("PerformBuild error: %v", err)
	}

	info, err := os.Stat(onDisk)
	if err != nil {
		t.Fatalf("Stat on-disk binary: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("expected replaced on-disk binary to remain executable, mode was %v", info.Mode())
	}
}

func TestPerformBuild_NoLeftoverStagingFile(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	if _, err := PerformBuild(s, nil); err != nil {
		t.Fatalf("PerformBuild error: %v", err)
	}

	if _, err := os.Stat(onDisk + ".new"); !os.IsNotExist(err) {
		t.Fatalf("expected no leftover staging file after a successful replace, stat err: %v", err)
	}
}

func TestPerformBuild_FirstBuild_CreatesDirAndBinary(t *testing.T) {
	// A brand-new source server that has never been built: the on-disk
	// binary AND its containing directory don't exist yet. PerformBuild
	// must create the directory and write the binary, not just fail
	// because the parent dir is missing.
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "nested", "does", "not", "exist", "server")

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	result, err := PerformBuild(s, nil)
	if err != nil {
		t.Fatalf("PerformBuild error on first-ever build: %v", err)
	}
	if !result.Replaced {
		t.Fatalf("expected Replaced=true — no prior binary existed")
	}
	if _, err := os.Stat(onDisk); err != nil {
		t.Fatalf("expected binary to now exist on disk at %q: %v", onDisk, err)
	}
}

func TestPerformBuild_NonSourceType_Errors(t *testing.T) {
	s := config.Server{
		Name:    "caddy",
		Type:    config.TypeExec,
		Command: "caddy",
	}

	_, err := PerformBuild(s, nil)
	if err == nil {
		t.Fatalf("expected loud error running build verb against a non-source server, got nil")
	}
	if !strings.Contains(err.Error(), "caddy") {
		t.Fatalf("expected error to name the server, got: %v", err)
	}
}

func TestPerformBuild_NeverStartsTheServer(t *testing.T) {
	// PerformBuild must not launch anything, regardless of whether the
	// binary was replaced — starting is up's job, per design/aa-server-status.md
	// §5. There is no direct way to assert "nothing was launched" other
	// than confirming PerformBuild's own signature/behavior never touches
	// a process; this test documents the contract by asserting the
	// BuildResult carries no process handle and completes without error
	// even though the (replaced) binary is now on disk but not running.
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	result, err := PerformBuild(s, nil)
	if err != nil {
		t.Fatalf("PerformBuild error: %v", err)
	}
	if !result.Replaced {
		t.Fatalf("expected Replaced=true for this fixture")
	}
	// BuildResult must carry exactly {Replaced, Restarted} — no process
	// handle fields. Code inspection confirms PerformBuild never calls
	// Launch; this assertion breaks if the struct grows a handle field.
	v := reflect.ValueOf(result)
	got := make([]string, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		got = append(got, v.Type().Field(i).Name)
	}
	want := []string{"Replaced", "Restarted"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildResult fields = %v, want exactly %v", got, want)
	}
}

// ---- PerformBuild: lifecycle mirroring (stop → replace → start) ----

func TestPerformBuild_StaleWithLifecycle_CallsStopBeforeReplace(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	var order []string
	lc := &BuildLifecycle{
		Stop:  func() error { order = append(order, "stop"); return nil },
		Start: func() error { order = append(order, "start"); return nil },
	}

	result, err := PerformBuild(s, lc)
	if err != nil {
		t.Fatalf("PerformBuild error: %v", err)
	}
	if !result.Replaced {
		t.Fatalf("expected Replaced=true")
	}
	if !result.Restarted {
		t.Fatalf("expected Restarted=true when lifecycle callbacks were provided")
	}
	if !slices.Equal(order, []string{"stop", "start"}) {
		t.Fatalf("expected [stop, start] call order, got %v", order)
	}
}

func TestPerformBuild_IdenticalWithLifecycle_NeitherStopNorStart(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v1", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	called := false
	lc := &BuildLifecycle{
		Stop:  func() error { called = true; return nil },
		Start: func() error { called = true; return nil },
	}

	result, err := PerformBuild(s, lc)
	if err != nil {
		t.Fatalf("PerformBuild error: %v", err)
	}
	if result.Replaced || result.Restarted {
		t.Fatalf("expected no-op for identical binary")
	}
	if called {
		t.Fatalf("stop/start must not be called when the binary is identical (no-op)")
	}
}

func TestPerformBuild_StaleWithNilLifecycle_JustReplaces(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	result, err := PerformBuild(s, nil)
	if err != nil {
		t.Fatalf("PerformBuild error: %v", err)
	}
	if !result.Replaced {
		t.Fatalf("expected Replaced=true")
	}
	if result.Restarted {
		t.Fatalf("expected Restarted=false when no lifecycle callbacks provided")
	}
}

func TestPerformBuild_StopError_AbortsBeforeReplace(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	before, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("ReadFile before: %v", err)
	}

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	lc := &BuildLifecycle{
		Stop:  func() error { return fmt.Errorf("stop failed: pid stuck") },
		Start: func() error { t.Fatal("start must not be called if stop fails"); return nil },
	}

	_, err = PerformBuild(s, lc)
	if err == nil {
		t.Fatalf("expected error when stop fails")
	}
	if !strings.Contains(err.Error(), "stop failed") {
		t.Fatalf("expected stop error to propagate, got: %v", err)
	}

	after, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("binary must not be replaced if stop fails")
	}
}

func TestPerformBuild_StartError_ReportsReplacedButNotRestarted(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "server")
	seedOnDiskBinary(t, "./testdata/buildable_v2", onDisk)

	s := buildFixture("server", "./testdata/buildable_v1", onDisk)

	lc := &BuildLifecycle{
		Stop:  func() error { return nil },
		Start: func() error { return fmt.Errorf("start failed: port in use") },
	}

	result, err := PerformBuild(s, lc)
	if err == nil {
		t.Fatalf("expected error when start fails")
	}
	if !result.Replaced {
		t.Fatalf("binary should still be replaced even if restart fails")
	}
	if result.Restarted {
		t.Fatalf("Restarted must be false when start fails")
	}
}
