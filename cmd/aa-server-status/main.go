// Command aa-server-status is the supervisor's operator REPL: normally a
// singleton process (enforced via an exclusive flock) that prints a status
// table on launch and accepts verbs at a "aa-server-status> " prompt. The
// only non-interactive path is --auto up|down, added for systemd/ops
// automation (AATK-135); --auto down skips the flock so it can tear down a
// fleet while --auto up holds the lock.
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
// path selected by --config and the auto mode (empty string when not in
// auto mode, "up" or "down" otherwise). It uses a fresh FlagSet (rather
// than the package-global flag.CommandLine) so it can be called repeatedly
// and in isolation from tests.
func parseFlags(args []string) (configPath, autoMode string, err error) {
	var basePath string
	var auto string
	fs := flag.NewFlagSet("aa-server-status", flag.ContinueOnError)
	fs.StringVar(&basePath, "config", defaultBasePath, "path to the TOML config file to load")
	fs.StringVar(&auto, "auto", "", "`mode`: up (start fleet and stay alive) or down (stop fleet and exit)")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if auto != "" && auto != "up" && auto != "down" {
		return "", "", fmt.Errorf("--auto: unknown mode %q (want up or down)", auto)
	}
	return basePath, auto, nil
}

func main() {
	basePath, autoMode, err := parseFlags(os.Args[1:])
	if err != nil {
		os.Exit(2)
	}

	cfg, err := config.Load(basePath)
	if err != nil {
		printErr("config error: %v", err)
		os.Exit(1)
	}

	lockPath := cfg.Supervisor.LockFile
	if lockPath == "" {
		lockPath = defaultLockPath
		if cfg.Supervisor.BaseDir != "" {
			lockPath = filepath.Join(cfg.Supervisor.BaseDir, lockPath)
		}
	}

	if autoMode == "down" {
		engine := NewEngine(cfg, nil, os.Stdout)
		if err := RunAuto("down", os.Stdout, engine, nil); err != nil {
			printErr("%v", err)
			os.Exit(1)
		}
		return
	}

	lock, err := AcquireLock(lockPath)
	if err != nil {
		printErr("%v", err)
		os.Exit(1)
	}
	defer lock.Release()

	if autoMode == "up" {
		engine := NewEngine(cfg, nil, os.Stdout)
		engine.WatchConfig(basePath)
		stop := make(chan struct{})
		go watchAutoSignals(stop)
		if err := RunAuto("up", os.Stdout, engine, stop); err != nil {
			printErr("%v", err)
			os.Exit(1)
		}
		return
	}

	// One buffered reader over stdin, shared by the REPL loop and the
	// engine's [server.prompt] path. Two buffers over the same stream race
	// each other for input (AATK-29), so this must stay a single instance.
	stdin := bufio.NewReader(os.Stdin)

	engine := NewEngine(cfg, stdin, os.Stdout)
	engine.WatchConfig(basePath)
	go watchSignals(os.Stdout, engine)
	if err := Run(stdin, os.Stdout, engine); err != nil {
		printErr("%v", err)
		os.Exit(1)
	}
}
