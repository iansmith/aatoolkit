package lifecycle

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/iansmith/aatoolkit/config"
)

// SourceCommand returns s.Binary and s.Args verbatim — source servers get no
// auto-appended flags (design/aa-server-status.md §4): run the binary with
// explicit args, ports come from the server's own args/config since a
// source server may be multi-port.
func SourceCommand(s config.Server) (command string, args []string) {
	return s.Binary, s.Args
}

// LaunchSource launches s (a source-type server) under logDir using the
// common launch core. binary + args are passed through verbatim
// (SourceCommand) — no auto-appended --host/--port flags.
//
// s.Dir is still passed through as the child's cmd.Dir (per
// design/aa-server-status.md §7), but a relative s.Binary is resolved to an
// absolute path first: per config/types.go's Server.Dir doc, Binary always
// lands relative to aa-server-status's own launch cwd (that's where the build
// machinery in this file writes it), not relative to s.Dir — s.Dir's
// pre-existing role here is only to tell `go build` where to find source
// (insertGoDirFlag). Without this, a relative Binary would silently resolve
// against s.Dir at exec time instead of the directory it was actually built into.
func LaunchSource(logDir string, s config.Server) (*Process, error) {
	command, args := SourceCommand(s)
	if command != "" && !filepath.IsAbs(command) {
		abs, err := filepath.Abs(command)
		if err != nil {
			return nil, fmt.Errorf("resolving binary path for %q: %w", s.Name, err)
		}
		command = abs
	}
	return launchWithCommand(logDir, s, command, args)
}

// expandTilde expands a leading "~" or "~/..." to the user's home
// directory. exec.Command never invokes a shell, so TOML strings like
// "~/slopstop/router" need this done explicitly — the OS/go tool won't do
// it for us.
func expandTilde(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expanding %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// insertGoDirFlag inserts "-C <dir>" (dir tilde-expanded) as the first flag
// after fields[0], per go's requirement that -C be the very first flag on
// the command line. It only changes where go looks for source — the
// build's actual -o output has already been rewritten to a temp path by
// the caller, so it's unaffected by the working-directory change -C makes.
// Requires fields[0] == "go" (enforced at config-validate time too, but
// checked again here since this is the actual point of use).
func insertGoDirFlag(fields []string, dir string) ([]string, error) {
	if len(fields) == 0 || fields[0] != "go" {
		return nil, fmt.Errorf("'dir' requires a 'go' build command, got %q", strings.Join(fields, " "))
	}
	expanded, err := expandTilde(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(fields)+2)
	out = append(out, fields[0], "-C", expanded)
	out = append(out, fields[1:]...)
	return out, nil
}

// RewriteBuildOutput replaces the "-o <path>" token pair in buildCmd with
// newPath, per design/aa-server-status.md §5: the canonical build command's
// output path is rewritten to a temp path for the staleness probe (and
// reused by the real rebuild).
//
// Only a literal, space-separated "-o" token followed by a path token
// counts as the pair — "-o=path" (single token) and long-flag forms like
// "--output" are NOT rewritten; both fall through to the same "missing -o"
// hard error, per the ticket's explicit token-pair rule.
func RewriteBuildOutput(buildCmd, newPath string) (string, error) {
	fields, err := rewriteBuildOutputFields(buildCmd, newPath)
	if err != nil {
		return "", err
	}
	return strings.Join(fields, " "), nil
}

// rewriteBuildOutputFields is RewriteBuildOutput's token-pair replace,
// operating on and returning the already-split fields — buildToTemp needs
// the fields form anyway (to insert -buildvcs=false), so this avoids a
// join-then-resplit round trip.
func rewriteBuildOutputFields(buildCmd, newPath string) ([]string, error) {
	fields := strings.Fields(buildCmd)

	for i, f := range fields {
		if f != "-o" {
			continue
		}
		if i+1 >= len(fields) {
			return nil, fmt.Errorf("build command %q: -o flag has no path argument", buildCmd)
		}
		fields[i+1] = newPath
		return fields, nil
	}

	return nil, fmt.Errorf("build command %q: missing required -o <path> flag", buildCmd)
}

// insertBuildVCSFalse inserts "-buildvcs=false" immediately after the
// rewritten "-o <path>" pair in fields (mutated in place order preserved via
// a fresh slice). It must land there rather than at the tail: `go build`
// stops flag parsing at the first non-flag argument (the package pattern),
// so appending -buildvcs=false after the package pattern would make the go
// tool treat it as an (invalid) additional package pattern instead of a
// flag. Assumes -o already appears in fields (call after a successful
// RewriteBuildOutput) and is a no-op if -buildvcs=false is already present
// anywhere in fields.
func insertBuildVCSFalse(fields []string) []string {
	for _, f := range fields {
		if f == "-buildvcs=false" || f == "-buildvcs" {
			return fields
		}
	}

	insertAt := len(fields)
	for i, f := range fields {
		if f == "-o" {
			insertAt = i + 2 // after "-o" and its path argument
			break
		}
	}

	out := make([]string, 0, len(fields)+1)
	out = append(out, fields[:insertAt]...)
	out = append(out, "-buildvcs=false")
	out = append(out, fields[insertAt:]...)
	return out
}

// buildToTemp runs s.Build with -o rewritten to a fresh temp path and
// -buildvcs=false inserted, per design/aa-server-status.md §5. It never touches
// the on-disk binary (s.Binary) — the build target is always the temp
// path. Returns the temp binary path and a cleanup func the caller must
// invoke when done with it (removes the whole temp directory).
//
// Each call performs a fresh build; there is no cross-call cache in this
// package. A cold build can take seconds (Go module/build-cache warmup);
// a session-level cache, if wanted, belongs to the long-lived supervisor
// process that owns call-site timing and lifetime, not this stateless
// helper package.
func buildToTemp(s config.Server) (tempPath string, cleanup func(), err error) {
	if strings.TrimSpace(s.Build) == "" {
		return "", nil, fmt.Errorf("server %q: type source requires a non-empty 'build' command", s.Name)
	}

	dir, err := os.MkdirTemp("", "server-build-"+s.Name+"-")
	if err != nil {
		return "", nil, fmt.Errorf("staleness probe for %q: creating temp dir: %w", s.Name, err)
	}
	cleanup = func() { os.RemoveAll(dir) }

	// filepath.Base never returns "" (it returns "." for an empty input),
	// so only "." (empty/degenerate Binary) and the separator itself
	// (Binary == "/") need the s.Name fallback.
	binaryName := filepath.Base(s.Binary)
	if binaryName == "." || binaryName == string(filepath.Separator) {
		binaryName = s.Name
	}
	tempPath = filepath.Join(dir, binaryName)

	fields, err := rewriteBuildOutputFields(s.Build, tempPath)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("server %q: %w", s.Name, err)
	}
	if s.Dir != "" {
		fields, err = insertGoDirFlag(fields, s.Dir)
		if err != nil {
			cleanup()
			return "", nil, fmt.Errorf("server %q: %w", s.Name, err)
		}
	}
	fields = insertBuildVCSFalse(fields)

	cmd := exec.Command(fields[0], fields[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("building %q (%s): %w\n%s", s.Name, strings.Join(fields, " "), err, out)
	}

	return tempPath, cleanup, nil
}

// hashFile returns the sha256 digest of the file at path.
func hashFile(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte

	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// filesIdentical reports whether a and b have identical content. A missing
// b (the common "never built before" case for the on-disk binary) counts
// as NOT identical, not an error — the caller (ProbeStaleness) surfaces
// that as stale.
func filesIdentical(a, b string) (bool, error) {
	hashA, err := hashFile(a)
	if err != nil {
		return false, err
	}

	hashB, err := hashFile(b)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	return hashA == hashB, nil
}

// StalenessResult is the outcome of a staleness probe for a source server.
type StalenessResult struct {
	// Stale is true when the on-disk binary differs from (or is missing
	// relative to) a fresh build of current source.
	Stale bool
	// TempBinary is the path to the freshly-built temp binary that
	// produced this result. Callers that need to act on staleness (e.g.
	// the build verb) can reuse it instead of building again — call
	// Cleanup when done with it.
	TempBinary string
	// Cleanup removes the temp build directory. Always call it (even on
	// Stale == false) once TempBinary is no longer needed.
	Cleanup func()
}

// ProbeStaleness runs the canonical build for s to a temp path (never
// touching the on-disk binary) and hashes the result against s.Binary, per
// design/aa-server-status.md §5. A missing on-disk binary counts as stale.
// Computed on `status` and on `<source> up` (callers, not this function).
//
// ProbeStaleness only applies to source-type servers; called with any
// other type it returns a loud error rather than silently probing nothing.
func ProbeStaleness(s config.Server) (StalenessResult, error) {
	if s.Type != config.TypeSource {
		return StalenessResult{}, fmt.Errorf("server %q: staleness is only defined for type source (got %q)", s.Name, s.Type)
	}

	tempPath, cleanup, err := buildToTemp(s)
	if err != nil {
		return StalenessResult{}, err
	}

	same, err := filesIdentical(tempPath, s.Binary)
	if err != nil {
		cleanup()
		return StalenessResult{}, fmt.Errorf("server %q: comparing build output to on-disk binary: %w", s.Name, err)
	}

	return StalenessResult{Stale: !same, TempBinary: tempPath, Cleanup: cleanup}, nil
}

// replaceBinary atomically replaces destPath with the content of tempPath,
// preserving tempPath's file mode (so a freshly-built executable stays
// executable). It stages the copy at destPath+".new" in destPath's own
// directory — the same filesystem as the deployed binary — then renames
// into place, avoiding the cross-device rename that would result from
// renaming directly out of a temp dir under os.TempDir(). destPath's
// directory is created if it doesn't exist yet (a source server's first
// ever build).
func replaceBinary(tempPath, destPath string) error {
	info, err := os.Stat(tempPath)
	if err != nil {
		return err
	}

	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	staging := destPath + ".new"
	if err := copyFile(tempPath, staging, info.Mode()); err != nil {
		return err
	}

	if err := os.Rename(staging, destPath); err != nil {
		os.Remove(staging)
		return err
	}
	return nil
}

// copyFile copies src to dst with the given mode, truncating dst if it
// already exists.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// BuildLifecycle lets the caller supply stop/start callbacks so PerformBuild
// can mirror the prior lifecycle when replacing a stale binary: was running
// → stop → replace → start; was down → replace, stay down. Pass nil (or
// leave the func fields nil) when the server isn't running — PerformBuild
// skips the callback and just replaces the file.
type BuildLifecycle struct {
	Stop  func() error
	Start func() error
}

// BuildResult describes what PerformBuild did to a source server's on-disk
// binary.
type BuildResult struct {
	// Replaced is true if the on-disk binary was rewritten because the
	// temp build differed from it (or it didn't exist). false means the
	// temp build was identical to what's already on disk — a no-op.
	Replaced bool
	// Restarted is true only when Replaced is true AND the caller supplied
	// a non-nil BuildLifecycle with both Stop and Start, and the full
	// stop → replace → start sequence completed without error.
	Restarted bool
}

// PerformBuild runs the `build` verb's core operation for a source server s
// (design/aa-server-status.md §5): build to temp, hash-compare against the
// on-disk binary, and — if different — atomically replace it, mirroring
// the prior lifecycle around the replacement:
//
//   - was running (lc.Stop != nil) → stop → replace → start
//   - was down (lc nil or lc.Stop nil) → replace, stay down
//
// build never starts a server that wasn't already running; starting from
// cold is up's job.
//
// PerformBuild applies only to source-type servers; called with any other
// type ("build" on a non-source server) it returns a loud error instead of
// silently doing nothing, matching design/aa-server-status.md §2's `build`
// verb contract.
func PerformBuild(s config.Server, lc *BuildLifecycle) (BuildResult, error) {
	if s.Type != config.TypeSource {
		return BuildResult{}, fmt.Errorf("server %q: build verb only applies to source servers (got type %q)", s.Name, s.Type)
	}

	probe, err := ProbeStaleness(s)
	if err != nil {
		return BuildResult{}, err
	}
	defer probe.Cleanup()

	if !probe.Stale {
		return BuildResult{Replaced: false}, nil
	}

	if lc != nil && lc.Stop != nil {
		if err := lc.Stop(); err != nil {
			return BuildResult{}, fmt.Errorf("stopping %q before binary replace: %w", s.Name, err)
		}
	}

	if err := replaceBinary(probe.TempBinary, s.Binary); err != nil {
		return BuildResult{}, fmt.Errorf("replacing binary for %q: %w", s.Name, err)
	}

	if lc != nil && lc.Start != nil {
		if err := lc.Start(); err != nil {
			return BuildResult{Replaced: true}, fmt.Errorf("restarting %q after binary replace: %w", s.Name, err)
		}
		return BuildResult{Replaced: true, Restarted: true}, nil
	}

	return BuildResult{Replaced: true}, nil
}
