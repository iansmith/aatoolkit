//go:build darwin || linux

package smsfake_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/iansmith/aatoolkit/telephony/twilio/smsfake"
)

// oneSocketEnv marks the child process
// TestNewCaptureServer_NeedsOnlyTheRequestedPortsSocket re-executes; only that
// child lowers its file-descriptor limit.
const oneSocketEnv = "AATOOLKIT_TEST_ONE_SOCKET"

// TestNewCaptureServer_NeedsOnlyTheRequestedPortsSocket pins that
// NewCaptureServer opens no socket but the listener on the requested port. A
// process that can open exactly one more descriptor, as one short of ports or
// descriptors may, must still get a working capture server on that port: not a
// panic from a second, ephemeral bind it never needed, with the requested
// port's listener left held behind it.
//
// Lowering the descriptor limit would starve every other test in this binary,
// so the test re-executes itself and lowers it only in that child.
func TestNewCaptureServer_NeedsOnlyTheRequestedPortsSocket(t *testing.T) {
	if os.Getenv(oneSocketEnv) == "1" {
		oneSocketChild(t)
		return
	}
	const name = "TestNewCaptureServer_NeedsOnlyTheRequestedPortsSocket"
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1", "-test.v", "-test.timeout=1m")
	cmd.Env = append(os.Environ(), oneSocketEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+name) {
		t.Fatalf("child process did not run the test:\n%s", out)
	}
}

// oneSocketChild is the child process's half of
// TestNewCaptureServer_NeedsOnlyTheRequestedPortsSocket.
func oneSocketChild(t *testing.T) {
	// freeTCPPort binds and closes a listener, so the one-time setup a first
	// bind does (the poller's own descriptor among it) is behind us before the
	// budget is spent.
	port := freeTCPPort(t)

	release := spendAllButOneDescriptor(t)
	var (
		capture  *smsfake.CaptureServer
		err      error
		panicked any
	)
	func() {
		defer func() { panicked = recover() }()
		capture, err = smsfake.NewCaptureServer(port)
	}()
	// Whatever NewCaptureServer did, the one spare descriptor is now spent: by
	// the listener it returned, or by one it leaked. If a further bind still
	// succeeds, there was more than one to spare, and a second bind inside
	// NewCaptureServer could have succeeded too, proving nothing.
	extra, extraErr := net.Listen("tcp", "127.0.0.1:0")
	release()
	if extraErr == nil {
		extra.Close()
		if capture != nil {
			capture.Close()
		}
		t.Fatalf("a bind after NewCaptureServer succeeded: the process had more than one descriptor to spare, so this run could not show NewCaptureServer needs only one")
	}

	switch {
	case panicked != nil:
		t.Errorf("NewCaptureServer(%d) panicked with one descriptor to spare, which the requested port's listener needs and nothing else should: %v", port, panicked)
	case err != nil:
		t.Errorf("NewCaptureServer(%d) = %v, want a server: the requested port was free and one descriptor was to spare for its listener", port, err)
	default:
		if want := fmt.Sprintf("127.0.0.1:%d", port); !strings.HasSuffix(capture.URL, want) {
			t.Errorf("capture server URL = %q, want it to end in %q", capture.URL, want)
		}
		capture.Close()
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d is still held once NewCaptureServer has returned and anything it returned is closed: %v", port, err)
	}
	ln.Close()
}

// spendAllButOneDescriptor lowers the process's descriptor limit and opens
// descriptors up to it, then closes one of them, so exactly one more can be
// opened until release is called. release closes the rest and restores the
// limit. Descriptors are taken lowest first, so filling to the limit leaves no
// free one below it but the one closed.
func spendAllButOneDescriptor(t *testing.T) (release func()) {
	t.Helper()
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("read the descriptor limit: %v", err)
	}
	probe, err := syscall.Open(os.DevNull, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	if err := syscall.Close(probe); err != nil {
		t.Fatalf("close %s: %v", os.DevNull, err)
	}

	// probe is the lowest free descriptor; a limit a little above it leaves a
	// handful to fill.
	lowered := limit
	lowered.Cur = uint64(probe) + 16
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lowered); err != nil {
		t.Fatalf("lower the descriptor limit to %d: %v", lowered.Cur, err)
	}

	var held []int
	for {
		fd, err := syscall.Open(os.DevNull, syscall.O_RDONLY, 0)
		if err == syscall.EMFILE {
			break
		}
		if err != nil {
			t.Fatalf("open %s while filling the descriptor table: %v", os.DevNull, err)
		}
		held = append(held, fd)
	}
	if len(held) == 0 {
		t.Fatalf("no descriptor could be opened under a limit of %d", lowered.Cur)
	}
	if err := syscall.Close(held[len(held)-1]); err != nil {
		t.Fatalf("free the one spare descriptor: %v", err)
	}
	held = held[:len(held)-1]

	return func() {
		for _, fd := range held {
			syscall.Close(fd)
		}
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
			t.Errorf("restore the descriptor limit to %d: %v", limit.Cur, err)
		}
	}
}
