package main

import (
	"fmt"
	"io"
	"os"
)

// ErrorMark is the unique token ops greps for in supervisor logs after a
// software_update. Every auto-mode failure line on stderr (and cold-down
// failures on the auto writer) carries it, so a playbook can scan ~/logs
// without matching ordinary status text.
const ErrorMark = "AASTATUS-ERROR"

func printErr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, ErrorMark+": "+format+"\n", args...)
}

func printErrTo(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, ErrorMark+": "+format+"\n", args...)
}
