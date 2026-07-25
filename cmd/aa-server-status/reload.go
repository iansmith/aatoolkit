package main

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/iansmith/aatoolkit/config"
)

// fileStamp is what "has this config file changed" is decided on: the file's
// modification time plus whether it exists at all. Existence is part of the
// stamp, not an afterthought — the local overlay is optional, and it appearing
// or disappearing changes the merged result just as much as an edit to it does.
type fileStamp struct {
	exists  bool
	modTime time.Time
}

func stampOf(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, modTime: info.ModTime()}
}

// configReloader watches the config file and re-loads it when it changes on
// disk. It watched a second, gitignored overlay alongside it until AATK-33
// deleted that mechanism; one path is now the whole story.
//
// Detection is mtime-based, per the ticket: a poll on the dispatch path, not a
// filesystem watcher. That makes the no-change case an os.Stat pair and nothing
// more — the check runs before every command typed at the prompt, so it has to
// stay cheap. The tradeoff is that a content change landing inside the
// filesystem's mtime resolution is invisible; on APFS that resolution is
// nanoseconds, so it is not a practical concern here.
type configReloader struct {
	path string

	stamp fileStamp

	// reloads counts config.Load calls the watcher triggered, successful or
	// not. It exists so "nothing is re-parsed when nothing changed" is an
	// assertable fact rather than a claim.
	reloads atomic.Int64
}

func newConfigReloader(path string) *configReloader {
	return &configReloader{path: path, stamp: stampOf(path)}
}

// changed reports whether the watched file differs from the last stamp taken,
// and updates the stamp. Updating on detection rather than after a
// successful load is deliberate: a config that fails to parse should be
// reported once, not re-reported before every subsequent command until the
// operator finishes fixing it.
func (r *configReloader) changed() bool {
	stamp := stampOf(r.path)
	if stamp == r.stamp {
		return false
	}
	r.stamp = stamp
	return true
}

// maybeReload re-reads the config when the watched file has changed, and
// installs it on engine only if it parses and validates. A failed reload is
// reported to out and otherwise changes nothing: the previously loaded config
// stays live and the REPL keeps going. Catching the operator midway through an
// edit is the common case here, not an exceptional one.
func (r *configReloader) maybeReload(out io.Writer, engine *RealEngine) {
	if !r.changed() {
		return
	}
	r.reloads.Add(1)

	cfg, err := config.Load(r.path)
	if err != nil {
		fmt.Fprintf(out, "config reload failed, keeping the previous config: %v\n", err)
		return
	}
	engine.ReplaceConfig(cfg)
}

// WatchConfig points the engine's reload check at the config file it was
// loaded from. Without it the engine simply never reloads, which is what every
// test that does not care about reloading gets by default.
//
// This is a separate call rather than a NewEngine parameter so the existing
// NewEngine(cfg) call sites stay valid, and so "not watching" is the
// zero-value behavior rather than something each caller must opt out of.
func (e *RealEngine) WatchConfig(path string) {
	e.reloader = newConfigReloader(path)
}

// ReplaceConfig atomically installs cfg as the engine's live configuration.
// In-flight readers holding an earlier snapshot finish against it coherently;
// the next reader sees the new one.
func (e *RealEngine) ReplaceConfig(cfg config.Config) {
	e.cfg.Store(&cfg)
}

// ReloadConfigIfChanged implements Engine. A no-op when WatchConfig was never
// called.
func (e *RealEngine) ReloadConfigIfChanged(out io.Writer) {
	if e.reloader == nil {
		return
	}
	e.reloader.maybeReload(out, e)
}

// reloadCount reports how many times the watcher has actually re-read the
// config.
func (e *RealEngine) reloadCount() int {
	if e.reloader == nil {
		return 0
	}
	return int(e.reloader.reloads.Load())
}
