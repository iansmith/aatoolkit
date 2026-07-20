package lifecycle

import "github.com/iansmith/aatoolkit/config"

// ExecCommand returns s.Command and s.Args verbatim, per
// design/aa-server-status.md §4: exec servers get no auto-appended flags — the
// server creates its own listeners (e.g. caddy).
func ExecCommand(s config.Server) (command string, args []string) {
	return s.Command, s.Args
}

// LaunchExec launches s (an exec-type server) under logDir using the common
// launch core. command + args are passed through verbatim (ExecCommand).
func LaunchExec(logDir string, s config.Server) (*Process, error) {
	command, args := ExecCommand(s)
	return launchWithCommand(logDir, s, command, args)
}
