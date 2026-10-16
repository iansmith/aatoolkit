// Package policy is an interpreted policy for recipe R2: its entrypoint takes
// a context and passes it explicitly, reaching the context-taking send by
// asserting host.ContextSender on the host.Host it is handed.
package policy

import (
	"context"
	"errors"

	"github.com/iansmith/aatoolkit/host"
)

// Turn streams one reply for window on the fast tier under ctx.
func Turn(ctx context.Context, h host.Host, window []byte) (string, error) {
	cs, ok := h.(host.ContextSender)
	if !ok {
		return "", errors.New("host does not implement host.ContextSender")
	}
	content, _, err := cs.SendStreamContext(ctx, window, "fast", func(string) {})
	return string(content), err
}
