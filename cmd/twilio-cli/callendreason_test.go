package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
)

// TestCallEndReason pins the softened wording: the known clean-end causes render
// as plain outcomes, and an unknown error falls through to its raw text.
func TestCallEndReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ctrl-c", context.Canceled, "stopped (Ctrl-C)"},
		{"eof", io.EOF, "server closed the connection"},
		{"unexpected-eof", io.ErrUnexpectedEOF, "server closed the connection"},
		{"econnreset", syscall.ECONNRESET, "server closed the connection"},
		{"epipe", syscall.EPIPE, "server closed the connection"},
		{"net-closed", net.ErrClosed, "server closed the connection"},
		// The real coder/websocket error when the peer drops the socket: EOF
		// wrapped in reader/frame-header context. It must soften, not leak the
		// library chain -- this is the exact string this change exists to fix.
		{"wrapped-eof", fmt.Errorf("failed to get reader: failed to read frame header: %w", io.EOF), "server closed the connection"},
		{"unknown", errors.New("some other failure"), "some other failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := callEndReason(tc.err); got != tc.want {
				t.Errorf("callEndReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
