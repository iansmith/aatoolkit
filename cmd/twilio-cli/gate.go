package main

import (
	"sync/atomic"
	"time"
)

// micGateHangover is how long past the end of the player's queued audio the
// mic stays shut.
const micGateHangover = 250 * time.Millisecond

// micGate is the half-duplex mic gate. See gate.go's doc in the
// implementation commit; this is the contract the phase-0 tests bind.
type micGate struct {
	shutUntilNanos atomic.Int64
}

func newMicGate() *micGate { return &micGate{} }

func (g *micGate) shutUntil(_ time.Time) {}

func (g *micGate) open() {}

func (g *micGate) shut(_ time.Time) bool { return false }

func (g *micGate) wrap(send func([]byte) error) func([]byte) error { return send }
