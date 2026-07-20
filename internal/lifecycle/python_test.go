package lifecycle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iansmith/aatoolkit/config"
)

// writeExecutable writes an executable file at path (creating parent dirs as
// needed) with the given shell-script content.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// fakePythonScript is a stand-in for <venv>/bin/python used by preflight's
// per-package import-check. It fails (nonzero exit) only for packages whose
// name appears in badPkgs, so tests can assert that a specific package is
// named as missing without needing a real Python interpreter.
func fakePythonScript(badPkgs ...string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# args: -c \"import <pkg>\"\n")
	b.WriteString("arg=\"$2\"\n")
	for _, pkg := range badPkgs {
		b.WriteString("case \"$arg\" in\n  *")
		b.WriteString(pkg)
		b.WriteString("*) exit 1 ;;\nesac\n")
	}
	b.WriteString("exit 0\n")
	return b.String()
}

// --- PythonCommand: entry resolution + auto-append -------------------------

func TestPythonCommand_ResolvesEntryFirstTokenAgainstVenvBin(t *testing.T) {
	s := config.Server{
		Name:  "voice-out",
		Type:  config.TypePython,
		Host:  "127.0.0.1",
		Port:  7788,
		Venv:  ".venv",
		Entry: "supertonic serve",
	}

	command, args := PythonCommand(s)

	if command != filepath.Join(".venv", "bin", "supertonic") {
		t.Fatalf("expected command %q, got %q", filepath.Join(".venv", "bin", "supertonic"), command)
	}
	want := []string{"serve", "--host", "127.0.0.1", "--port", "7788"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected args %v, got %v", want, args)
	}
}

func TestPythonCommand_ResolvesEntryWithPythonInterpreterToken(t *testing.T) {
	s := config.Server{
		Name:  "voice-in",
		Type:  config.TypePython,
		Host:  "127.0.0.1",
		Port:  7789,
		Venv:  ".venv",
		Entry: "python scripts/whisper_server.py",
	}

	command, args := PythonCommand(s)

	if command != filepath.Join(".venv", "bin", "python") {
		t.Fatalf("expected command %q, got %q", filepath.Join(".venv", "bin", "python"), command)
	}
	want := []string{"scripts/whisper_server.py", "--host", "127.0.0.1", "--port", "7789"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected args %v, got %v", want, args)
	}
}

func TestPythonCommand_NoExtraFlagsBeyondHostPort(t *testing.T) {
	s := config.Server{
		Name:  "voice-out",
		Type:  config.TypePython,
		Host:  "0.0.0.0",
		Port:  9999,
		Venv:  "/opt/venv",
		Entry: "supertonic serve",
	}

	_, args := PythonCommand(s)

	if len(args) != 5 {
		t.Fatalf("expected exactly 5 args (serve, --host, host, --port, port), got %d: %v", len(args), args)
	}
	for _, a := range args {
		if a == "--verbose" || a == "--debug" {
			t.Fatalf("unexpected auto-appended flag %q in args %v", a, args)
		}
	}
}

func TestPythonCommand_EntryWithMultipleExtraTokens_PreservesOrder(t *testing.T) {
	s := config.Server{
		Name:  "voice-in",
		Type:  config.TypePython,
		Host:  "127.0.0.1",
		Port:  7789,
		Venv:  ".venv",
		Entry: "python -m scripts.whisper_server --reload",
	}

	command, args := PythonCommand(s)

	if command != filepath.Join(".venv", "bin", "python") {
		t.Fatalf("expected command %q, got %q", filepath.Join(".venv", "bin", "python"), command)
	}
	want := []string{"-m", "scripts.whisper_server", "--reload", "--host", "127.0.0.1", "--port", "7789"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected args %v, got %v", want, args)
	}
}

func TestPythonCommand_EntryWithSingleToken_NoExtraArgsBeforeHostPort(t *testing.T) {
	s := config.Server{
		Name:  "voice-out",
		Type:  config.TypePython,
		Host:  "127.0.0.1",
		Port:  7788,
		Venv:  ".venv",
		Entry: "supertonic",
	}

	command, args := PythonCommand(s)

	if command != filepath.Join(".venv", "bin", "supertonic") {
		t.Fatalf("expected command %q, got %q", filepath.Join(".venv", "bin", "supertonic"), command)
	}
	want := []string{"--host", "127.0.0.1", "--port", "7788"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("expected args %v (no extra tokens before host/port), got %v", want, args)
	}
}

func TestPythonCommand_VenvJoinUsesFullVenvPath(t *testing.T) {
	s := config.Server{
		Name:  "voice-out",
		Type:  config.TypePython,
		Host:  "127.0.0.1",
		Port:  7788,
		Venv:  "/abs/path/.venv",
		Entry: "supertonic serve",
	}

	command, _ := PythonCommand(s)

	want := filepath.Join("/abs/path/.venv", "bin", "supertonic")
	if command != want {
		t.Fatalf("expected command %q, got %q", want, command)
	}
}

// --- LaunchPython: preflight ------------------------------------------------

func TestLaunchPython_MissingVenvDir_ReturnsErrorNamingVenvPath(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	venv := filepath.Join(dir, "no-such-venv")

	s := config.Server{
		Name:  "voice-out",
		Type:  config.TypePython,
		Host:  "127.0.0.1",
		Port:  7788,
		Venv:  venv,
		Entry: "supertonic serve",
	}

	proc, err := LaunchPython(logDir, s)
	if err == nil {
		t.Fatalf("expected an error for a missing venv dir, got nil (proc=%v)", proc)
	}
	if proc != nil {
		t.Fatalf("expected nil Process on preflight failure, got %v", proc)
	}
	if !strings.Contains(err.Error(), venv) {
		t.Fatalf("expected error to name the venv path %q, got: %v", venv, err)
	}
}

func TestLaunchPython_MissingEntryBinary_ReturnsErrorNamingIt(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	venv := filepath.Join(dir, ".venv")
	if err := os.MkdirAll(filepath.Join(venv, "bin"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// venv/bin exists but "supertonic" is not installed in it.

	s := config.Server{
		Name:  "voice-out",
		Type:  config.TypePython,
		Host:  "127.0.0.1",
		Port:  7788,
		Venv:  venv,
		Entry: "supertonic serve",
	}

	proc, err := LaunchPython(logDir, s)
	if err == nil {
		t.Fatalf("expected an error for a missing entry binary, got nil (proc=%v)", proc)
	}
	if proc != nil {
		t.Fatalf("expected nil Process on preflight failure, got %v", proc)
	}
	wantPath := filepath.Join(venv, "bin", "supertonic")
	if !strings.Contains(err.Error(), wantPath) {
		t.Fatalf("expected error to name the missing binary path %q, got: %v", wantPath, err)
	}
}

func TestLaunchPython_EntryBinaryIsDirectory_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	venv := filepath.Join(dir, ".venv")

	// "supertonic" resolves to a directory, not an executable file — a stale
	// or half-finished install. os.Stat alone would see it as "existing";
	// preflight must reject it anyway since it can't be launched.
	if err := os.MkdirAll(filepath.Join(venv, "bin", "supertonic"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	s := config.Server{
		Name:  "voice-out",
		Type:  config.TypePython,
		Host:  "127.0.0.1",
		Port:  7788,
		Venv:  venv,
		Entry: "supertonic serve",
	}

	proc, err := LaunchPython(logDir, s)
	if err == nil {
		t.Fatalf("expected an error when the resolved entry path is a directory, got nil (proc=%v)", proc)
	}
	if proc != nil {
		t.Fatalf("expected nil Process on preflight failure, got %v", proc)
	}
	wantPath := filepath.Join(venv, "bin", "supertonic")
	if !strings.Contains(err.Error(), wantPath) {
		t.Fatalf("expected error to name the bad entry path %q, got: %v", wantPath, err)
	}
}

func TestLaunchPython_MissingPythonBin_ReturnsInterpreterNotFound(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	venv := filepath.Join(dir, ".venv")

	// Entry binary present, but <venv>/bin/python does NOT exist.
	writeExecutable(t, filepath.Join(venv, "bin", "supertonic"), "#!/bin/sh\nexit 0\n")

	s := config.Server{
		Name:     "voice-out",
		Type:     config.TypePython,
		Host:     "127.0.0.1",
		Port:     7788,
		Venv:     venv,
		Entry:    "supertonic serve",
		Packages: []string{"supertonic"},
	}

	proc, err := LaunchPython(logDir, s)
	if err == nil {
		t.Fatalf("expected an error for missing python interpreter, got nil (proc=%v)", proc)
	}
	if proc != nil {
		t.Fatalf("expected nil Process on preflight failure, got %v", proc)
	}
	if !strings.Contains(err.Error(), "python interpreter not found") {
		t.Fatalf("expected error to say 'python interpreter not found', got: %v", err)
	}
	if !strings.Contains(err.Error(), venv) {
		t.Fatalf("expected error to name the venv path, got: %v", err)
	}
}

func TestLaunchPython_MissingPackage_NamesSpecificPackage(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	venv := filepath.Join(dir, ".venv")

	// Entry binary present.
	writeExecutable(t, filepath.Join(venv, "bin", "supertonic"), "#!/bin/sh\nexit 0\n")
	// Fake python fails the import check only for "supertonic".
	writeExecutable(t, filepath.Join(venv, "bin", "python"), fakePythonScript("supertonic"))

	s := config.Server{
		Name:     "voice-out",
		Type:     config.TypePython,
		Host:     "127.0.0.1",
		Port:     7788,
		Venv:     venv,
		Entry:    "supertonic serve",
		Packages: []string{"supertonic"},
	}

	proc, err := LaunchPython(logDir, s)
	if err == nil {
		t.Fatalf("expected an error for a missing package, got nil (proc=%v)", proc)
	}
	if proc != nil {
		t.Fatalf("expected nil Process on preflight failure, got %v", proc)
	}
	if !strings.Contains(err.Error(), "supertonic") {
		t.Fatalf("expected error to name the specific missing package %q, got: %v", "supertonic", err)
	}
}

func TestLaunchPython_MultiplePackages_ChecksEachIndividually(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	venv := filepath.Join(dir, ".venv")

	// entry IS python; fastapi/uvicorn/multipart importable, mlx_whisper is not.
	writeExecutable(t, filepath.Join(venv, "bin", "python"), fakePythonScript("mlx_whisper"))

	s := config.Server{
		Name:     "voice-in",
		Type:     config.TypePython,
		Host:     "127.0.0.1",
		Port:     7789,
		Venv:     venv,
		Entry:    "python scripts/whisper_server.py",
		Packages: []string{"fastapi", "uvicorn", "mlx_whisper", "multipart"},
	}

	proc, err := LaunchPython(logDir, s)
	if err == nil {
		t.Fatalf("expected an error — mlx_whisper is not importable, got nil (proc=%v)", proc)
	}
	if proc != nil {
		t.Fatalf("expected nil Process on preflight failure, got %v", proc)
	}
	if !strings.Contains(err.Error(), "mlx_whisper") {
		t.Fatalf("expected error to name the specific missing package %q (not a generic combined-import failure), got: %v", "mlx_whisper", err)
	}
	// Must not misreport one of the genuinely-importable packages instead.
	if strings.Contains(err.Error(), "fastapi") || strings.Contains(err.Error(), "uvicorn") {
		t.Fatalf("expected only the missing package named, but error also blamed an importable one: %v", err)
	}
}

func TestLaunchPython_AllPreflightChecksPass_Launches(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	venv := filepath.Join(dir, ".venv")

	writeExecutable(t, filepath.Join(venv, "bin", "supertonic"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(venv, "bin", "python"), fakePythonScript()) // no bad packages

	s := config.Server{
		Name:     "voice-out",
		Type:     config.TypePython,
		Host:     "127.0.0.1",
		Port:     7788,
		Venv:     venv,
		Entry:    "supertonic serve",
		Packages: []string{"supertonic"},
	}

	proc, err := LaunchPython(logDir, s)
	if err != nil {
		t.Fatalf("expected preflight to pass and launch to succeed, got error: %v", err)
	}
	if proc == nil {
		t.Fatalf("expected a non-nil Process on success")
	}
	defer proc.Cmd.Process.Kill()

	if !strings.HasPrefix(proc.LogPath, logDir) {
		t.Fatalf("expected log path under %q, got %q", logDir, proc.LogPath)
	}
}

// TestLaunchPython_AllPreflightChecksPass_UsesResolvedCommand guards against
// a LaunchPython that reports success without actually wiring PythonCommand's
// resolved command/args through to Launch (e.g. a hardcoded or wrong
// invocation would still return a non-nil Process with err == nil).
func TestLaunchPython_AllPreflightChecksPass_UsesResolvedCommand(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	venv := filepath.Join(dir, ".venv")

	// Echoes its own args so the log file proves what was actually run.
	writeExecutable(t, filepath.Join(venv, "bin", "supertonic"), "#!/bin/sh\necho GOT_ARGS:\"$@\"\n")
	writeExecutable(t, filepath.Join(venv, "bin", "python"), fakePythonScript())

	s := config.Server{
		Name:     "voice-out",
		Type:     config.TypePython,
		Host:     "127.0.0.1",
		Port:     7788,
		Venv:     venv,
		Entry:    "supertonic serve",
		Packages: []string{"supertonic"},
	}

	proc, err := LaunchPython(logDir, s)
	if err != nil {
		t.Fatalf("expected preflight to pass and launch to succeed, got error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	content := waitForLogContent(t, proc.LogPath, "GOT_ARGS:")
	want := "GOT_ARGS:serve --host 127.0.0.1 --port 7788"
	if !strings.Contains(content, want) {
		t.Fatalf("expected launched process to have been invoked with resolved args %q, log contained: %q", want, content)
	}
}

// TestLaunchPython_RelativeVenvResolvesAgainstDir guards against a
// regression: pythonPreflight used to os.Stat a relative Venv/entry against
// the process's own cwd, while Launch's cmd.Dir (set from s.Dir) makes the
// actual exec resolve the same relative command against s.Dir instead — per
// design/aa-server-status.md §7's worked example, a relative venv is meant to
// resolve against Dir. Without the fix, this config would fail preflight
// even though the venv genuinely exists (just not relative to cwd).
func TestLaunchPython_RelativeVenvResolvesAgainstDir(t *testing.T) {
	serverDir := t.TempDir() // s.Dir — where the relative venv actually lives
	logDir := t.TempDir()

	writeExecutable(t, filepath.Join(serverDir, ".venv", "bin", "supertonic"), "#!/bin/sh\necho relative-venv-marker\n")
	writeExecutable(t, filepath.Join(serverDir, ".venv", "bin", "python"), fakePythonScript())

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	unrelatedCwd := t.TempDir() // must NOT be where the venv is looked up
	if err := os.Chdir(unrelatedCwd); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer os.Chdir(origWd)

	s := config.Server{
		Name:     "voice-out",
		Type:     config.TypePython,
		Host:     "127.0.0.1",
		Port:     7788,
		Dir:      serverDir,
		Venv:     ".venv",
		Entry:    "supertonic serve",
		Packages: []string{"supertonic"},
	}

	proc, err := LaunchPython(logDir, s)
	if err != nil {
		t.Fatalf("expected preflight to resolve venv against Dir and succeed, got error: %v", err)
	}
	defer proc.Cmd.Process.Kill()

	content := waitForLogContent(t, proc.LogPath, "relative-venv-marker")
	if !strings.Contains(content, "relative-venv-marker") {
		t.Fatalf("expected launched process output, got log: %q", content)
	}
}

func TestLaunchPython_NoPackagesDeclared_SkipsImportChecks(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	venv := filepath.Join(dir, ".venv")

	writeExecutable(t, filepath.Join(venv, "bin", "supertonic"), "#!/bin/sh\nexit 0\n")
	// Deliberately no venv/bin/python at all — if the implementation still
	// tried to import-check with zero packages it would fail spuriously.

	s := config.Server{
		Name:  "voice-out",
		Type:  config.TypePython,
		Host:  "127.0.0.1",
		Port:  7788,
		Venv:  venv,
		Entry: "supertonic serve",
		// Packages intentionally empty.
	}

	proc, err := LaunchPython(logDir, s)
	if err != nil {
		t.Fatalf("expected success with no packages declared, got error: %v", err)
	}
	if proc == nil {
		t.Fatalf("expected a non-nil Process on success")
	}
	defer proc.Cmd.Process.Kill()
}
