package driver

import (
	"bufio"
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
