// Package policy is an interpreted policy for recipe R1: it never names a
// context. It sends through the host.Host it is handed, and the caller decides
// which context that send runs under by handing it a WithContext view.
package policy

import "github.com/iansmith/aatoolkit/host"

// Turn streams one reply for window on the fast tier.
func Turn(h host.Host, window []byte) (string, error) {
	content, _, err := h.SendStream(window, "fast", func(string) {})
	return string(content), err
}
