// Command vadspike is a throwaway harness for proving out dual-threshold VAD
// segmentation — it is not part of any server build or fleet. Run it directly:
// go run ./cmd/vadspike
package main

import (
	"fmt"
	"os"

	"github.com/iansmith/aatoolkit/driver"
)

func main() {
	if err := driver.RunVADSpike(); err != nil {
		fmt.Fprintln(os.Stderr, "vadspike:", err)
		os.Exit(1)
	}
}
