package main

import (
	"fmt"
	"io"
)

// RunAuto runs the non-interactive auto mode: "up" brings the enabled fleet
// up and blocks until stop is closed (a signal in production); "down" stops
// the enabled fleet and returns immediately. Errors from the engine are
// returned so the caller can exit non-zero.
func RunAuto(mode string, out io.Writer, engine Engine, stop <-chan struct{}) error {
	switch mode {
	case "up":
		engine.ReloadConfigIfChanged(out)
		if err := engine.Up(""); err != nil {
			return err
		}
		printStatus(out, engine.Status())
		<-stop
		teardown(out, engine)
		return nil
	case "down":
		return coldDown(out, engine)
	default:
		panic(fmt.Sprintf("RunAuto: invalid mode %q; parseFlags must reject this", mode))
	}
}

// coldDown iterates enabled servers by name and calls Down(name) for
// each. The per-server path in the engine discovers running processes
// by declared port when it does not own them — so a cold-start
// process (no lock, empty procs map) can still tear down a fleet
// started by another supervisor instance.
func coldDown(out io.Writer, engine Engine) error {
	var firstErr error
	for _, s := range engine.Status() {
		if !s.Enabled {
			continue
		}
		if err := engine.Down(s.Name); err != nil {
			printErrTo(out, "down %s: %v", s.Name, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
