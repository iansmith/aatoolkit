package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// sigintWindow is how long a burst of Ctrl-C presses has to reach three
// before the supervisor treats it as a deliberate "shut down now" request.
const sigintWindow = 2 * time.Second

// interruptHint is printed on the first and second SIGINT within the
// window, telling the operator how to actually shut things down.
const interruptHint = "interrupt ignored — Ctrl-C ×3 within 2s shuts down"

// sigintCounter tracks a rolling burst of SIGINT (Ctrl-C) deliveries. A
// burst that reaches 3 within sigintWindow completes the shutdown request;
// a delivery arriving after the window has elapsed since the burst's first
// delivery starts a brand-new burst instead of extending the stale one.
type sigintCounter struct {
	count int
	first time.Time
}

// register records one SIGINT arriving at now and reports whether this
// delivery completes a 3x-within-window burst. A completed burst resets
// the counter so a later, independent burst needs its own fresh 3
// deliveries rather than immediately re-triggering.
func (c *sigintCounter) register(now time.Time) bool {
	if c.count == 0 || now.Sub(c.first) > sigintWindow {
		c.count = 1
		c.first = now
		return false
	}
	c.count++
	if c.count >= 3 {
		c.count = 0
		return true
	}
	return false
}

// handleSignal processes one signal delivery. SIGTSTP is always swallowed
// with zero side effects — the supervisor must never be suspended out from
// under its children. Any other signal (SIGINT, delivered as os.Interrupt)
// registers against c: the 1st/2nd within the window print the swallow
// hint, and the 3rd runs the exact same teardown path as the "bye" verb
// (repl.go's teardown) before reporting true so the caller can exit.
func handleSignal(sig os.Signal, c *sigintCounter, now time.Time, out io.Writer, engine Engine) bool {
	if sig == syscall.SIGTSTP {
		return false
	}
	if c.register(now) {
		teardown(out, engine)
		return true
	}
	fmt.Fprintln(out, interruptHint)
	return false
}

// runSignalLoop consumes signals from sigCh until a 3x-within-window SIGINT
// burst triggers shutdown, at which point it calls exit and returns
// immediately without draining any further buffered signals.
func runSignalLoop(sigCh <-chan os.Signal, out io.Writer, engine Engine, exit func(int)) {
	counter := &sigintCounter{}
	for sig := range sigCh {
		if handleSignal(sig, counter, time.Now(), out, engine) {
			exit(0)
			return
		}
	}
}

// watchSignals installs the SIGINT/SIGTSTP handling described in SOP-18:
// SIGTSTP is trapped and ignored, and a burst of 3 SIGINTs within ~2s
// tears down everything the supervisor owns (the same path as "bye") and
// exits. Intended to run on its own goroutine from main, independent of
// whatever the REPL's blocking stdin read is doing.
func watchSignals(out io.Writer, engine Engine) {
	// Buffered generously: os/signal never blocks sending to this channel,
	// so a too-small buffer risks silently dropping a delivery if two
	// SIGINTs arrive before the loop drains the first — exactly the rapid
	// burst this feature exists to catch reliably.
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTSTP)
	runSignalLoop(sigCh, out, engine, os.Exit)
}
