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
		return engine.Down("")
	default:
		panic(fmt.Sprintf("RunAuto: invalid mode %q; parseFlags must reject this", mode))
	}
}
