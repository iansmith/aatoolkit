package driver

import (
	"bufio"
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
