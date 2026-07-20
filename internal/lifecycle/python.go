package lifecycle

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iansmith/aatoolkit/config"
)

// PythonCommand builds the launch command for a python-type server, per
// design/aa-server-status.md §4, §10.
//
// s.Entry's first whitespace-separated token is resolved against
// <venv>/bin (e.g. "supertonic serve" -> <venv>/bin/supertonic, with
// "serve" as the first arg; "python scripts/whisper_server.py" ->
// <venv>/bin/python, with "scripts/whisper_server.py" as the first arg).
// Any remaining tokens are appended verbatim, then --host/--port are
// auto-appended — no other flags are added.
func PythonCommand(s config.Server) (command string, args []string) {
	fields := strings.Fields(s.Entry)

	var token string
	var rest []string
	if len(fields) > 0 {
		token, rest = fields[0], fields[1:]
	}

	command = filepath.Join(s.Venv, "bin", token)

	args = make([]string, 0, len(rest)+4)
	args = append(args, rest...)
	args = append(args, "--host", s.Host, "--port", strconv.Itoa(s.Port))
	return command, args
}

// LaunchPython launches s (a python-type server) under logDir, after
// running the venv/package preflight described in design/aa-server-status.md
// §4, §10:
//
//   - the venv directory and the resolved <venv>/bin/<entry-token> must
//     exist (as a directory and a regular file, respectively);
//   - each entry in s.Packages is import-checked individually via
//     <venv>/bin/python -c "import <pkg>", so a failure names the exact
//     missing package rather than a combined check.
//
// A preflight failure returns a descriptive error naming what's missing
// and the venv path — it never launches, and it never crashes the caller;
// per §6.5, runtime command errors are the caller's job to surface loudly
// and return to the prompt.
func LaunchPython(logDir string, s config.Server) (*Process, error) {
	command, args := PythonCommand(s)

	if err := pythonPreflight(s, command); err != nil {
		return nil, err
	}

	return launchWithCommand(logDir, s, command, args)
}

// pythonPreflight checks that s's venv and resolved entry binary exist, and
// that every declared package is importable by the venv's own Python. When
// s.Dir is set, Launch's cmd.Dir resolves a relative command against s.Dir
// (not the supervisor's own cwd) — per design/aa-server-status.md §7's worked
// example, a relative venv/entry is meant to resolve the same way, so
// preflight resolves every path against the same expanded s.Dir the actual
// exec will use, instead of the process's own cwd.
func pythonPreflight(s config.Server, command string) error {
	base := ""
	if s.Dir != "" {
		expanded, err := expandTilde(s.Dir)
		if err != nil {
			return fmt.Errorf("python preflight for %q: expanding dir %q: %w", s.Name, s.Dir, err)
		}
		base = expanded
	}
	resolve := func(p string) string {
		if base == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}

	venvPath := resolve(s.Venv)
	venvInfo, err := os.Stat(venvPath)
	if err != nil {
		return fmt.Errorf("python preflight for %q: venv not found at %s: %w", s.Name, venvPath, err)
	}
	if !venvInfo.IsDir() {
		return fmt.Errorf("python preflight for %q: venv path %s is not a directory", s.Name, venvPath)
	}

	cmdPath := resolve(command)
	cmdInfo, err := os.Stat(cmdPath)
	if err != nil {
		return fmt.Errorf("python preflight for %q: entry binary not found at %s (venv %s); install it in the venv and retry", s.Name, cmdPath, venvPath)
	}
	if cmdInfo.IsDir() {
		return fmt.Errorf("python preflight for %q: entry path %s is a directory, not an executable (venv %s)", s.Name, cmdPath, venvPath)
	}

	if len(s.Packages) == 0 {
		return nil
	}

	pythonBin := filepath.Join(venvPath, "bin", "python")
	if cmdPath != pythonBin {
		pyInfo, err := os.Stat(pythonBin)
		if err != nil {
			return fmt.Errorf("python preflight for %q: python interpreter not found at %s (venv %s)", s.Name, pythonBin, venvPath)
		}
		if pyInfo.IsDir() {
			return fmt.Errorf("python preflight for %q: python path %s is a directory, not an executable (venv %s)", s.Name, pythonBin, venvPath)
		}
	}
	for _, pkg := range s.Packages {
		importCheck := exec.Command(pythonBin, "-c", "import "+pkg)
		if err := importCheck.Run(); err != nil {
			return fmt.Errorf("python preflight for %q: package %q not importable in venv %s (%s -c \"import %s\": %w)",
				s.Name, pkg, venvPath, pythonBin, pkg, err)
		}
	}
	return nil
}
