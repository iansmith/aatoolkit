package main

import "io"

// RunAuto runs the non-interactive auto mode: "up" brings the enabled fleet
// up and blocks until stop is closed (a signal in production); "down" stops
// the enabled fleet and returns immediately. Errors from the engine are
// returned so the caller can exit non-zero.
func RunAuto(mode string, out io.Writer, engine Engine, stop <-chan struct{}) error {
	return nil
}
