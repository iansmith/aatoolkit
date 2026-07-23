package lifecycle

import (
	"strconv"

	"github.com/iansmith/aatoolkit/config"
)

// MLXCommand builds the mlx-serve invocation for s, per
// design/aa-server-status.md §4:
//
//	mlx-serve serve <model> --host <host> --port <port> [--draft-model <model>]
//
// host/port are auto-appended; --draft-model is appended only when
// s.DraftModel is set (speculative decoding). No other flags are added.
func MLXCommand(s config.Server) (command string, args []string) {
	args = []string{
		"serve",
		s.Model,
		"--host", s.Host,
		"--port", strconv.Itoa(s.Port),
	}
	if s.DraftModel != "" {
		args = append(args, "--draft-model", s.DraftModel)
	}
	return "mlx-serve", args
}

// LaunchMLX launches s (an mlx-type server) under logDir using the common
// launch core.
func LaunchMLX(logDir string, s config.Server) (*Process, error) {
	command, args := MLXCommand(s)
	return launchWithCommand(logDir, s, command, args)
}
