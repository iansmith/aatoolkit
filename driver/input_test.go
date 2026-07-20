package driver

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestStdinSourceReadsTrimmedLines(t *testing.T) {
	s := &stdinSource{sc: bufio.NewScanner(strings.NewReader("  hello world  \n/reload\n"))}

	turn, ok, err := s.Next()
	if err != nil || !ok {
		t.Fatalf("first Next() = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if turn.Text != "hello world" {
		t.Fatalf("first turn = %q, want %q (line not trimmed)", turn.Text, "hello world")
	}
	turn2, ok2, _ := s.Next()
	if !ok2 || turn2.Text != "/reload" {
		t.Fatalf("second turn = (%q, %v), want (\"/reload\", true)", turn2.Text, ok2)
	}
	if _, ok3, _ := s.Next(); ok3 {
		t.Fatalf("third Next() ok = true, want false at EOF")
	}
}

// DoD-critical: typed lines (commands + text) must pass through untouched in
// voice mode — a non-empty line must NOT trigger a mic capture.
func TestVoiceSourceTypedLinesBypassCapture(t *testing.T) {
	captured := false
	v := &voiceSource{
		sc:         bufio.NewScanner(strings.NewReader("/reload\n")),
		capture:    func() ([]byte, error) { captured = true; return nil, nil },
		transcribe: func([]byte) (string, error) { return "SHOULD NOT BE CALLED", nil },
	}
	turn, ok, err := v.Next()
	if err != nil || !ok || turn.Text != "/reload" {
		t.Fatalf("typed turn = (%q, %v, %v), want (\"/reload\", true, nil)", turn.Text, ok, err)
	}
	if captured {
		t.Fatal("a typed line triggered a mic capture — it must pass through instead")
	}
}

// A bare Enter records + transcribes; the transcript becomes the turn, and the
// captured audio is what gets transcribed.
func TestVoiceSourceEmptyLineCapturesAndTranscribes(t *testing.T) {
	v := &voiceSource{
		sc:      bufio.NewScanner(strings.NewReader("\n")),
		capture: func() ([]byte, error) { return []byte("RIFFfakeaudio"), nil },
		transcribe: func(wav []byte) (string, error) {
			if string(wav) != "RIFFfakeaudio" {
				return "", errors.New("captured audio was not passed to transcribe")
			}
			return "turn the lights on", nil
		},
	}
	turn, ok, err := v.Next()
	if err != nil || !ok {
		t.Fatalf("Next() = (_, %v, %v), want ok + nil err", ok, err)
	}
	if turn.Text != "turn the lights on" {
		t.Fatalf("turn = %q, want the transcript %q", turn.Text, "turn the lights on")
	}
}

// A capture failure surfaces as an error, not a silent empty turn.
func TestVoiceSourceCaptureErrorSurfaces(t *testing.T) {
	v := &voiceSource{
		sc:         bufio.NewScanner(strings.NewReader("\n")),
		capture:    func() ([]byte, error) { return nil, errors.New("mic unavailable") },
		transcribe: func([]byte) (string, error) { return "", nil },
	}
	if _, _, err := v.Next(); err == nil {
		t.Fatal("expected an error when capture fails, got nil")
	}
}

// Silence → empty transcript → an empty turn (the loop reprompts), not an error.
func TestVoiceSourceEmptyTranscriptIsEmptyTurn(t *testing.T) {
	v := &voiceSource{
		sc:         bufio.NewScanner(strings.NewReader("\n")),
		capture:    func() ([]byte, error) { return []byte("RIFF"), nil },
		transcribe: func([]byte) (string, error) { return "", nil },
	}
	turn, ok, err := v.Next()
	if err != nil || !ok {
		t.Fatalf("Next() = (_, %v, %v), want ok + nil err", ok, err)
	}
	if turn.Text != "" {
		t.Fatalf("turn = %q, want empty for a silent capture", turn.Text)
	}
}

// Adversary gap: a transcribe failure must surface as an error (symmetry with
// the capture-error case), not be swallowed into an empty turn.
func TestVoiceSourceTranscribeErrorSurfaces(t *testing.T) {
	v := &voiceSource{
		sc:         bufio.NewScanner(strings.NewReader("\n")),
		capture:    func() ([]byte, error) { return []byte("RIFF"), nil },
		transcribe: func([]byte) (string, error) { return "", errors.New("stt down") },
	}
	if _, _, err := v.Next(); err == nil {
		t.Fatal("expected the transcribe error to surface, got nil")
	}
}

// Adversary gap: a mixed typed/spoken/typed stream advances turn-by-turn,
// capture fires exactly once (only for the bare Enter), and EOF ends input
// without a capture.
func TestVoiceSourceSequenceMixedAndCaptureOnce(t *testing.T) {
	n := 0
	v := &voiceSource{
		sc:         bufio.NewScanner(strings.NewReader("/reload\n\nhello\n")),
		capture:    func() ([]byte, error) { n++; return []byte("RIFF"), nil },
		transcribe: func([]byte) (string, error) { return "spoken", nil },
	}
	t1, ok1, _ := v.Next() // typed → passthrough, no capture
	t2, ok2, _ := v.Next() // empty → capture once → "spoken"
	t3, ok3, _ := v.Next() // typed → passthrough, no capture
	_, ok4, _ := v.Next()  // EOF
	if !ok1 || !ok2 || !ok3 || ok4 {
		t.Fatalf("ok flags = %v,%v,%v,%v; want true,true,true,false", ok1, ok2, ok3, ok4)
	}
	if t1.Text != "/reload" || t2.Text != "spoken" || t3.Text != "hello" {
		t.Fatalf("sequence = %q, %q, %q; want /reload, spoken, hello", t1.Text, t2.Text, t3.Text)
	}
	if n != 1 {
		t.Fatalf("capture called %d times, want exactly 1 (only the bare Enter)", n)
	}
}

// Adversary gap: a whitespace-only line trims to empty, so it's treated as a
// bare Enter (capture) — consistent with how stdinSource trims.
func TestVoiceSourceWhitespaceOnlyLineCaptures(t *testing.T) {
	captured := false
	v := &voiceSource{
		sc:         bufio.NewScanner(strings.NewReader("   \n")),
		capture:    func() ([]byte, error) { captured = true; return []byte("RIFF"), nil },
		transcribe: func([]byte) (string, error) { return "spoken", nil },
	}
	turn, _, _ := v.Next()
	if !captured || turn.Text != "spoken" {
		t.Fatalf("whitespace-only line → (%q, captured=%v); want trimmed→capture→\"spoken\"", turn.Text, captured)
	}
}

// Adversary gap: a typed line is trimmed like stdinSource does, or command
// dispatch ("  /reload  ") would break.
func TestVoiceSourceTrimsTypedLine(t *testing.T) {
	captured := false
	v := &voiceSource{
		sc:         bufio.NewScanner(strings.NewReader("  /reload  \n")),
		capture:    func() ([]byte, error) { captured = true; return nil, nil },
		transcribe: func([]byte) (string, error) { return "", nil },
	}
	turn, _, _ := v.Next()
	if turn.Text != "/reload" || captured {
		t.Fatalf("typed turn = %q (captured=%v); want trimmed \"/reload\", no capture", turn.Text, captured)
	}
}

// Item 5: a no-speech capture (silence / wrong device / stall) is an empty turn
// that reprompts, and must NOT spend a transcription.
func TestVoiceSourceNoSpeechIsEmptyTurn(t *testing.T) {
	transcribed := false
	v := &voiceSource{
		sc:         bufio.NewScanner(strings.NewReader("\n")),
		capture:    func() ([]byte, error) { return nil, errNoSpeech },
		transcribe: func([]byte) (string, error) { transcribed = true; return "x", nil },
	}
	turn, ok, err := v.Next()
	if !ok || err != nil || turn.Text != "" {
		t.Fatalf("no-speech turn = (%q, ok=%v, err=%v), want empty turn, ok=true, nil err", turn.Text, ok, err)
	}
	if transcribed {
		t.Fatal("a no-speech capture must skip transcription")
	}
}

// Adversary: errNoSpeech may arrive wrapped, so the check must use errors.Is,
// not ==.
func TestVoiceSourceWrappedNoSpeechIsEmptyTurn(t *testing.T) {
	v := &voiceSource{
		sc:         bufio.NewScanner(strings.NewReader("\n")),
		capture:    func() ([]byte, error) { return nil, fmt.Errorf("device stalled: %w", errNoSpeech) },
		transcribe: func([]byte) (string, error) { t.Fatal("must not transcribe on no-speech"); return "", nil },
	}
	turn, ok, err := v.Next()
	if !ok || err != nil || turn.Text != "" {
		t.Fatalf("wrapped errNoSpeech = (%q, ok=%v, err=%v), want empty turn, ok=true, nil err", turn.Text, ok, err)
	}
}
