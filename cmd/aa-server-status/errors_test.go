package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestErrorMark_IsStableAndGrepFriendly(t *testing.T) {
	if ErrorMark != "AASTATUS-ERROR" {
		t.Errorf("ErrorMark = %q, want AASTATUS-ERROR — ops/software_update greps this exact token", ErrorMark)
	}
	if strings.Contains(ErrorMark, " ") {
		t.Error("ErrorMark contains a space; grep -F would still work but the token must stay one field")
	}
}

func TestPrintErr_PrefixesTheMark(t *testing.T) {
	var buf bytes.Buffer
	printErrTo(&buf, "down %s: %v", "web", "boom")
	got := buf.String()
	if !strings.HasPrefix(got, ErrorMark+": ") {
		t.Errorf("printErrTo = %q, want it to start with %q", got, ErrorMark+": ")
	}
	if !strings.Contains(got, "down web: boom") {
		t.Errorf("printErrTo = %q, want the formatted message after the mark", got)
	}
}
