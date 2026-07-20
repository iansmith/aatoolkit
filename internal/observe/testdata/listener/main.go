// Command listener is a test fixture for internal/observe. It binds to one
// TCP port per -port flag, optionally spawns a child listener process (for
// process-tree tests), and blocks until killed. It is built by the observe
// package tests via `go build` into a temp dir and exec'd as a real child
// process — this is the "process the test spawns itself" fixture called for
// by SOP-15 ("unit-testable against processes the test spawns itself").
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	var ports string
	var childPorts string
	var grandchildPorts string
	var transientChild bool
	var exitAfterMS int
	flag.StringVar(&ports, "ports", "", "comma-separated TCP ports to listen on")
	flag.StringVar(&childPorts, "child-ports", "", "comma-separated TCP ports for a spawned child listener (optional)")
	flag.StringVar(&grandchildPorts, "grandchild-ports", "", "comma-separated TCP ports for the child's own spawned child (optional; requires -child-ports)")
	flag.BoolVar(&transientChild, "transient-child", false, "also spawn a short-lived, non-listening child that exits almost immediately (races the tree walk)")
	flag.IntVar(&exitAfterMS, "exit-after-ms", 0, "if set, bind nothing and exit after this many milliseconds instead of blocking")
	flag.Parse()

	if exitAfterMS > 0 {
		time.Sleep(time.Duration(exitAfterMS) * time.Millisecond)
		return
	}

	if transientChild {
		// -exit-after-ms makes this invocation bind nothing and exit almost
		// immediately, so it is a real child of this process that may
		// already be gone by the time the observer's tree walk reaches it.
		transient := spawnSelf("-exit-after-ms", "50")
		go transient.Wait() // reap without blocking startup
	}

	var listeners []net.Listener
	for _, p := range splitNonEmpty(ports) {
		port, err := strconv.Atoi(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad port %q: %v\n", p, err)
			os.Exit(1)
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "listen on %d: %v\n", port, err)
			os.Exit(1)
		}
		listeners = append(listeners, l)
	}

	if childPorts != "" {
		args := []string{"-ports", childPorts}
		if grandchildPorts != "" {
			args = append(args, "-child-ports", grandchildPorts)
		}
		spawnSelf(args...)
	}

	// Signal readiness on stdout so the test can synchronize instead of
	// polling on a timer alone.
	fmt.Println("ready")

	// Block forever; the test kills this process (and its child, if any)
	// via process-group signaling. A bare `select{}` (or a receive on a
	// channel nobody can send to) triggers Go's all-goroutines-asleep
	// deadlock detector, which panics instead of blocking — so wait on a
	// real OS signal instead, which the runtime treats as a live external
	// event.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
}

// spawnSelf re-execs this same binary with args, wiring its stdout/stderr to
// ours, and exits the whole fixture process on failure. Used both for a real
// nested child listener (-child-ports) and for the transient, non-listening
// child (-exit-after-ms) that races the tree walk.
func spawnSelf(args ...string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "os.Executable: %v\n", err)
		os.Exit(1)
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "spawnSelf %v: %v\n", args, err)
		os.Exit(1)
	}
	return cmd
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
