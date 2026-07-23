package lifecycle

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/iansmith/aatoolkit/config"
)

// MLXCommand builds the mlx-serve invocation for s, per
// design/aa-server-status.md §4:
//
//	mlx-serve serve <model> --host <host> --port <port> [--drafter <path>]
//
// host/port are auto-appended; --drafter is appended only when s.Drafter is
// set (Gemma-4-style assistant-checkpoint speculative decoding). No other
// flags are added.
//
// Confirmed against a real mlx-serve 26.7.8 install: passing --model as a
// short "org/repo" name together with any --drafter value (short name or
// path) fails with a spurious FileNotFound — mlx-serve's own discovery
// registry excludes drafter/assistant checkpoints (their model_type isn't a
// servable chat model), so its short-name resolution can't validate the
// pairing. Passing BOTH as full local paths under mlx-serve's own models
// directory sidesteps that resolution path entirely and works. When
// s.Drafter is set, resolve both s.Model and s.Drafter to their cached local
// path if one exists; a name with no matching cached directory (or when
// s.Drafter is unset) is passed through unchanged, exactly as before.
func MLXCommand(s config.Server) (command string, args []string) {
	model := s.Model
	drafter := s.Drafter
	if drafter != "" {
		if root, err := mlxModelsRoot(); err == nil {
			model = resolveMLXModelPath(root, model)
			drafter = resolveMLXModelPath(root, drafter)
		}
	}
	args = []string{
		"serve",
		model,
		"--host", s.Host,
		"--port", strconv.Itoa(s.Port),
	}
	if drafter != "" {
		args = append(args, "--drafter", drafter)
	}
	return "mlx-serve", args
}

// mlxModelsRoot returns mlx-serve's own local model cache directory
// (~/.mlx-serve/models), the same directory `mlx-serve pull`/`list` use.
func mlxModelsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mlx-serve", "models"), nil
}

// resolveMLXModelPath expands a short "org/repo" mlx-serve model identifier
// to its full local path under root, if that path is actually a cached
// directory on disk. An already-absolute name, or a name with no matching
// local directory, is returned unchanged.
func resolveMLXModelPath(root, name string) string {
	if root == "" || name == "" || filepath.IsAbs(name) {
		return name
	}
	candidate := filepath.Join(root, name)
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return name
}

// LaunchMLX launches s (an mlx-type server) under logDir using the common
// launch core.
func LaunchMLX(logDir string, s config.Server) (*Process, error) {
	command, args := MLXCommand(s)
	return launchWithCommand(logDir, s, command, args)
}
