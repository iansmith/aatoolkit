// Command aa-server-status is the supervisor's operator REPL: a singleton
// process (enforced via an exclusive flock) that prints a status table on
// launch and accepts verbs at a "aa-server-status> " prompt. There is no
// one-shot CLI grammar — every verb is typed at the prompt.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iansmith/aatoolkit/config"
)

const (
	defaultLockPath  = "build/run/aa-server-status.lock"
	defaultBasePath  = "aa-server-status.toml"
	defaultLocalPath = "aa-server-status.local.toml"
)

// parseFlags parses command-line arguments and returns the base config
// path selected by --config, defaulting to defaultBasePath when the flag
// is omitted. It uses a fresh FlagSet (rather than the package-global
// flag.CommandLine) so it can be called repeatedly and in isolation from
// tests.
func parseFlags(args []string) (string, error) {
	var basePath string
	fs := flag.NewFlagSet("aa-server-status", flag.ContinueOnError)
	fs.StringVar(&basePath, "config", defaultBasePath, "path to the TOML config file to load")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return basePath, nil
}

// localConfigPath derives the local overlay path from the base config
// path by convention: a ".toml" suffix is swapped for ".local.toml";
// otherwise ".local.toml" is appended. The overlay file itself remains
// optional — config.Load skips it if it doesn't exist.
func localConfigPath(basePath string) string {
	if strings.HasSuffix(basePath, ".toml") {
		return strings.TrimSuffix(basePath, ".toml") + ".local.toml"
	}
	return basePath + ".local.toml"
}

func main() {
	basePath, err := parseFlags(os.Args[1:])
	if err != nil {
		os.Exit(2)
	}
	localPath := localConfigPath(basePath)

	cfg, err := config.Load(basePath, localPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aa-server-status: config error: %v\n", err)
		os.Exit(1)
	}

	lockPath := cfg.Supervisor.LockFile
	if lockPath == "" {
		lockPath = defaultLockPath
		if cfg.Supervisor.BaseDir != "" {
			lockPath = filepath.Join(cfg.Supervisor.BaseDir, lockPath)
		}
	}

	lock, err := AcquireLock(lockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aa-server-status: %v\n", err)
		os.Exit(1)
	}
	defer lock.Release()

	engine := NewEngine(cfg)
	go watchSignals(os.Stdout, engine)
	if err := Run(os.Stdin, os.Stdout, engine); err != nil {
		fmt.Fprintf(os.Stderr, "aa-server-status: %v\n", err)
		os.Exit(1)
	}
}
