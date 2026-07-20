// Command tdlistener is a test fixture for internal/lifecycle's teardown
// tests. It binds one TCP port, optionally serves a trivial HTTP 200 health
// endpoint on that same port, optionally spawns a child listener (so the
// group-kill can be exercised against a real multi-process tree), and either
// exits cleanly on SIGTERM (the graceful-teardown path) or ignores SIGTERM
// entirely (forcing the caller down the SIGKILL path) depending on
// -ignore-term. It is built via `go build` and exec'd as a real child
// process — teardown tests must spawn actual processes and actually kill
// them, never mock syscall.Kill.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var port int
	var childPort int
	var ignoreTerm bool
	var serveHealth bool
	flag.IntVar(&port, "port", 0, "TCP port to listen on")
	flag.IntVar(&childPort, "child-port", 0, "if set, also spawn a child listening on this port")
	flag.BoolVar(&ignoreTerm, "ignore-term", false, "ignore SIGTERM (and SIGINT) instead of exiting, forcing the caller to SIGKILL")
	flag.BoolVar(&serveHealth, "serve-health", false, "serve a 200 OK at /healthz on the listened port")
	flag.Parse()

	if port == 0 {
		fmt.Fprintln(os.Stderr, "tdlistener: -port is required")
		os.Exit(1)
	}

	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "tdlistener: listen on %d: %v\n", port, err)
		os.Exit(1)
	}

	if serveHealth {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		go http.Serve(l, mux)
	}

	if childPort != 0 {
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "tdlistener: os.Executable: %v\n", err)
			os.Exit(1)
		}
		args := []string{"-port", fmt.Sprint(childPort)}
		if ignoreTerm {
			args = append(args, "-ignore-term")
		}
		if serveHealth {
			args = append(args, "-serve-health")
		}
		child := exec.Command(self, args...)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "tdlistener: spawn child: %v\n", err)
			os.Exit(1)
		}
		go child.Wait() // reap without blocking
	}

	// Signal readiness on stdout so the test can synchronize instead of
	// polling on a timer alone.
	fmt.Println("ready")

	sigCh := make(chan os.Signal, 1)
	if ignoreTerm {
		// Deliberately ignore SIGTERM/SIGINT: the caller's grace-period wait
		// will elapse with this process (and its listener) still alive,
		// forcing the SIGKILL branch. SIGKILL itself cannot be caught or
		// ignored, so that's what actually ends this process. Block on a
		// receive from a channel nobody sends to (rather than `select{}`,
		// which Go's runtime treats as a whole-program deadlock and
		// crashes with "all goroutines are asleep") — signal.Notify below
		// is never reached, but keeping a goroutine registered via a timer
		// tick prevents the deadlock detector from firing.
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
		block := make(chan struct{})
		go func() {
			for {
				time.Sleep(time.Hour)
			}
		}()
		<-block
	}
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	// Graceful path: exit promptly (closing the listener) once signaled,
	// simulating a well-behaved server that honors SIGTERM.
	time.Sleep(20 * time.Millisecond)
}
