package driver

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Turn is one unit of user input. Text is the typed line or the transcript;
// Speaker is who said it (empty until speaker-ID lands — SOP-2 keeps it "").
type Turn struct {
	Text    string
	Speaker string
}

// inputSource yields user turns. The driver owns it; the policy never sees it.
// Next returns (turn, ok, err); ok=false means end of input (EOF / quit).
type inputSource interface {
	Next() (Turn, bool, error)
}

// stdinSource reads typed lines — today's behavior.
type stdinSource struct{ sc *bufio.Scanner }

func (s *stdinSource) Next() (Turn, bool, error) {
	if !s.sc.Scan() {
		return Turn{}, false, s.sc.Err()
	}
	return Turn{Text: strings.TrimSpace(s.sc.Text())}, true, nil
}

// voiceSource keeps typed input working (so /reload etc. still work) but treats
// an empty line (a bare Enter) as "capture a spoken turn": record via capture,
// transcribe, and return the transcript as the turn. capture/transcribe are
// injected so the behavior is testable without a mic or a server.
type voiceSource struct {
	sc         *bufio.Scanner
	capture    func() ([]byte, error)
	transcribe func(wav []byte) (string, error)
}

func (v *voiceSource) Next() (Turn, bool, error) {
	if !v.sc.Scan() {
		return Turn{}, false, v.sc.Err()
	}
	// A non-empty typed line passes straight through — commands and text both
	// keep working with voice input on.
	if line := strings.TrimSpace(v.sc.Text()); line != "" {
		return Turn{Text: line}, true, nil
	}
	// A bare Enter records a spoken turn.
	wav, err := v.capture()
	if errors.Is(err, errNoSpeech) {
		fmt.Fprintln(os.Stderr, "(no speech detected — check the mic if this repeats)")
		return Turn{}, true, nil // empty turn: reprompt without spending a transcription
	}
	if err != nil {
		return Turn{}, true, fmt.Errorf("capture: %w", err)
	}
	fmt.Println("⏳  Transcribing…")
	text, err := v.transcribe(wav)
	if err != nil {
		return Turn{}, true, fmt.Errorf("transcribe: %w", err)
	}
	if text != "" {
		fmt.Printf("(heard) %s\n", text)
	} else {
		fmt.Println("(heard nothing usable — try again)")
	}
	return Turn{Text: text}, true, nil
}
