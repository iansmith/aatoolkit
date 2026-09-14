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
	engine.ReloadConfigIfChanged(out)

	switch mode {
	case "up":
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
		return fmt.Errorf("--auto: unknown mode %q (want up or down)", mode)
	}
}
