// Command aa-server-status is the supervisor's operator REPL: a singleton
// process (enforced via an exclusive flock) that prints a status table on
// launch and accepts verbs at a "aa-server-status> " prompt. There is no
// one-shot CLI grammar — every verb is typed at the prompt.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/iansmith/aatoolkit/config"
)

const (
	defaultLockPath = "build/run/aa-server-status.lock"
	defaultBasePath = "aa-server-status.toml"
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

func main() {
	basePath, err := parseFlags(os.Args[1:])
	if err != nil {
		os.Exit(2)
	}

	cfg, err := config.Load(basePath)
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

	// One buffered reader over stdin, shared by the REPL loop and the
	// engine's [server.prompt] path. Two buffers over the same stream race
	// each other for input (AATK-29), so this must stay a single instance.
	stdin := bufio.NewReader(os.Stdin)

	engine := NewEngine(cfg, stdin, os.Stdout)
	engine.WatchConfig(basePath)
	go watchSignals(os.Stdout, engine)
	if err := Run(stdin, os.Stdout, engine); err != nil {
		fmt.Fprintf(os.Stderr, "aa-server-status: %v\n", err)
		os.Exit(1)
	}
}
