package driver

import (
	"testing"
	"time"
)

// helpers to keep the span sequences readable
const (
	tArm     = 2 * time.Second
	tAbs     = 8 * time.Second
	tMaxDone = 1 * time.Second
)

// --- endpointer state machine ---

// Happy path: talk, pause ~2s (arm), a short "Done" clip isolated by silence →
// confirm; confirming it as the stopword ends the turn.
func TestEndpointerArmsThenConfirmsDone(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	if d := e.onSpan(true, 3*time.Second); d != keepRecording {
		t.Fatalf("while talking: got %v, want keepRecording", d)
	}
	if d := e.onSpan(false, 2*time.Second); d != keepRecording {
		t.Fatalf("2s pause (arming): got %v, want keepRecording", d)
	}
	if d := e.onSpan(true, 400*time.Millisecond); d != keepRecording {
		t.Fatalf("short clip (not yet isolated): got %v, want keepRecording", d)
	}
	if d := e.onSpan(false, 400*time.Millisecond); d != confirmCandidate {
		t.Fatalf("short clip isolated by silence: got %v, want confirmCandidate", d)
	}
	if d := e.confirmed(true); d != endTurn {
		t.Fatalf("clip was the stopword: got %v, want endTurn", d)
	}
}

// Edge: a >2s think-pause followed by a LONG continuation is not a "Done" — the
// turn keeps going, and a later real gesture still ends it.
func TestEndpointerThinkPauseDoesNotEnd(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	e.onSpan(true, 2*time.Second)
	e.onSpan(false, 2500*time.Millisecond) // >2s pause → armed
	if d := e.onSpan(true, 2*time.Second); d != keepRecording {
		t.Fatalf("long continuation after pause: got %v, want keepRecording (not a Done)", d)
	}
	if d := e.onSpan(false, 2*time.Second); d != keepRecording {
		t.Fatalf("re-arming pause: got %v, want keepRecording", d)
	}
	e.onSpan(true, 300*time.Millisecond)
	if d := e.onSpan(false, 300*time.Millisecond); d != confirmCandidate {
		t.Fatalf("short clip after the second pause: got %v, want confirmCandidate", d)
	}
}

// Error/rejection: a candidate that transcribes to something other than the
// stopword resumes listening (it was real speech), and a later gesture ends it.
func TestEndpointerConfirmNoMatchResumesListening(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	e.onSpan(true, 2*time.Second)
	e.onSpan(false, 2*time.Second)
	e.onSpan(true, 400*time.Millisecond)
	if d := e.onSpan(false, 400*time.Millisecond); d != confirmCandidate {
		t.Fatalf("first candidate: got %v, want confirmCandidate", d)
	}
	if d := e.confirmed(false); d != keepRecording {
		t.Fatalf("not the stopword: got %v, want keepRecording", d)
	}
	e.onSpan(true, 1*time.Second)        // the real continued speech
	e.onSpan(false, 2*time.Second)       // a FRESH arm-length pause is required
	e.onSpan(true, 300*time.Millisecond) // the actual "Done" this time
	if d := e.onSpan(false, 300*time.Millisecond); d != confirmCandidate {
		t.Fatalf("second candidate after a fresh arm: got %v, want confirmCandidate", d)
	}
}

// Error/rejection: if "Done" is never said, a long enough continuous silence
// ends the turn anyway (fallback).
func TestEndpointerAbsoluteSilenceFallback(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	e.onSpan(true, 2*time.Second)
	if d := e.onSpan(false, 8*time.Second); d != endTurn {
		t.Fatalf("8s continuous silence: got %v, want endTurn", d)
	}
}

// Boundary: a short clip after a sub-arm pause (<2s) is NOT a candidate; only a
// short clip after a full arm-length pause is.
func TestEndpointerSilenceBelowArmIsNotACandidate(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	e.onSpan(true, 2*time.Second)
	e.onSpan(false, 1500*time.Millisecond) // < arm
	e.onSpan(true, 300*time.Millisecond)   // short, but the pause was too short
	if d := e.onSpan(false, 300*time.Millisecond); d != keepRecording {
		t.Fatalf("short clip after sub-arm pause: got %v, want keepRecording (not a candidate)", d)
	}
	e.onSpan(true, 500*time.Millisecond) // more talk
	e.onSpan(false, 2*time.Second)       // now a full arm-length pause
	e.onSpan(true, 300*time.Millisecond)
	if d := e.onSpan(false, 300*time.Millisecond); d != confirmCandidate {
		t.Fatalf("short clip after full arm pause: got %v, want confirmCandidate", d)
	}
}

// Adversary: the maxDoneClip boundary — a clip of exactly maxDoneClip is still a
// "Done" candidate (<=); one tick longer is a continuation.
func TestEndpointerMaxDoneClipBoundary(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	e.onSpan(true, 2*time.Second)
	e.onSpan(false, tArm)
	e.onSpan(true, tMaxDone) // exactly maxDoneClip
	if d := e.onSpan(false, 400*time.Millisecond); d != confirmCandidate {
		t.Fatalf("clip == maxDoneClip: got %v, want confirmCandidate", d)
	}
	e2 := newEndpointer(tArm, tAbs, tMaxDone)
	e2.onSpan(true, 2*time.Second)
	e2.onSpan(false, tArm)
	e2.onSpan(true, tMaxDone+time.Millisecond) // one tick over
	if d := e2.onSpan(false, 400*time.Millisecond); d != keepRecording {
		t.Fatalf("clip > maxDoneClip: got %v, want keepRecording (continuation)", d)
	}
}

// Adversary: the armSilence boundary — a pause of exactly armSilence arms (>=);
// one tick under does not.
func TestEndpointerArmBoundary(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	e.onSpan(true, 2*time.Second)
	e.onSpan(false, tArm) // exactly armSilence
	e.onSpan(true, 300*time.Millisecond)
	if d := e.onSpan(false, 400*time.Millisecond); d != confirmCandidate {
		t.Fatalf("pause == armSilence should arm: got %v, want confirmCandidate", d)
	}
	e2 := newEndpointer(tArm, tAbs, tMaxDone)
	e2.onSpan(true, 2*time.Second)
	e2.onSpan(false, tArm-time.Millisecond) // one tick under
	e2.onSpan(true, 300*time.Millisecond)
	if d := e2.onSpan(false, 400*time.Millisecond); d != keepRecording {
		t.Fatalf("pause < armSilence should not arm: got %v, want keepRecording", d)
	}
}

// Adversary: the machine latches after endTurn — later spans stay terminal.
func TestEndpointerLatchesAfterEndTurn(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	e.onSpan(true, 2*time.Second)
	if e.onSpan(false, 8*time.Second) != endTurn {
		t.Fatal("setup: expected endTurn on absolute silence")
	}
	if d := e.onSpan(true, 300*time.Millisecond); d != endTurn {
		t.Fatalf("span after endTurn should stay terminal: got %v, want endTurn", d)
	}
}

// Adversary: after a false alarm, a fresh arm-length pause is required — a short
// clip without one is not a candidate.
func TestEndpointerNoMatchRequiresFreshArm(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	e.onSpan(true, 2*time.Second)
	e.onSpan(false, tArm)
	e.onSpan(true, 400*time.Millisecond)
	if e.onSpan(false, 400*time.Millisecond) != confirmCandidate {
		t.Fatal("setup: expected confirmCandidate")
	}
	e.confirmed(false)                   // false alarm → back to LISTENING
	e.onSpan(true, 300*time.Millisecond) // speech, no fresh arm-length pause
	if d := e.onSpan(false, 500*time.Millisecond); d != keepRecording {
		t.Fatalf("candidate without a fresh arm after no-match: got %v, want keepRecording", d)
	}
}

// Review fix: leading silence before any speech must NOT arm — otherwise the
// first short word becomes a spurious "Done" candidate.
func TestEndpointerLeadingSilenceDoesNotArm(t *testing.T) {
	e := newEndpointer(tArm, tAbs, tMaxDone)
	e.onSpan(false, tArm)                // silence before the speaker has said anything
	e.onSpan(true, 300*time.Millisecond) // first short word
	if d := e.onSpan(false, 400*time.Millisecond); d != keepRecording {
		t.Fatalf("leading silence armed the machine: first word got %v, want keepRecording", d)
	}
}

// --- isStopword ---

func TestIsStopword(t *testing.T) {
	words := []string{"done", "stop"}
	cases := []struct {
		text string
		want bool
	}{
		{"done", true},
		{"Done.", true},
		{"  STOP!  ", true},
		{"stopping", false}, // not exactly "stop"
		{"i'm done", false}, // more than the bare word
		{"milk", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isStopword(c.text, words); got != c.want {
			t.Errorf("isStopword(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// Adversary: a stopword-list entry may itself carry punctuation, and an empty
// list must safely not match.
func TestIsStopwordListPunctuationAndEmpty(t *testing.T) {
	if !isStopword("done", []string{"done."}) {
		t.Error(`list entry "done." should match input "done"`)
	}
	if isStopword("done", nil) {
		t.Error("empty stopword list must not match")
	}
}

// --- wavHead ---

func TestWavHead(t *testing.T) {
	full := makeWAV(make([]int16, 16000)) // 1.0s at 16 kHz mono

	head := wavHead(full, 500*time.Millisecond)
	data, ok := wavData(head)
	if !ok {
		t.Fatal("wavHead output is not a parseable WAV")
	}
	if got := len(data) / 2; got != 8000 {
		t.Fatalf("wavHead(0.5s) has %d samples, want 8000", got)
	}

	// Truncating beyond the clip length yields the whole clip.
	whole, ok := wavData(wavHead(full, 2*time.Second))
	if !ok || len(whole)/2 != 16000 {
		t.Fatalf("wavHead(2s) of a 1s clip: got %d samples (ok=%v), want 16000", len(whole)/2, ok)
	}
}

// Adversary: d=0 yields a valid but empty WAV; a non-WAV input yields nil.
func TestWavHeadZeroAndGarbage(t *testing.T) {
	full := makeWAV(make([]int16, 16000))
	z, ok := wavData(wavHead(full, 0))
	if !ok || len(z) != 0 {
		t.Fatalf("wavHead(0): got %d data bytes (ok=%v), want 0 / true", len(z), ok)
	}
	if got := wavHead([]byte("not a wav at all"), 500*time.Millisecond); got != nil {
		t.Fatalf("wavHead(non-WAV) = %v, want nil", got)
	}
}
