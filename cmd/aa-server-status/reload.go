package main

import (
	"io"

	"github.com/iansmith/aatoolkit/config"
)

// ReplaceConfig is the NAIVE pre-AATK-27 implementation: a plain unsynchronized
// assignment to the shared cfg field. It exists in this state deliberately so
// the Phase 0 race test fails under -race for the real reason — an unguarded
// write racing Up's per-target readers — rather than passing vacuously against
// a no-op. AATK-27 replaces the field with an atomic.Pointer.
func (e *RealEngine) ReplaceConfig(cfg config.Config) { e.cfg = cfg }

// WatchConfig is a stub until AATK-27 implements it.
func (e *RealEngine) WatchConfig(basePath, localPath string) {}

// ReloadConfigIfChanged is a stub until AATK-27 implements it.
func (e *RealEngine) ReloadConfigIfChanged(out io.Writer) {}

// reloadCount is a stub until AATK-27 implements it.
func (e *RealEngine) reloadCount() int { return -1 }
