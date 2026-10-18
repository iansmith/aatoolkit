// Package policy is an interpreted policy for recipe R2: its entrypoints take
// a context and pass it explicitly, reaching the context-taking forms by
// asserting host.ContextSender and host.ContextBuilder on the host.Host they
// are handed.
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

// Window returns the messages array for queryText, built under ctx.
func Window(ctx context.Context, h host.Host, queryText string) ([]byte, error) {
	cb, ok := h.(host.ContextBuilder)
	if !ok {
		return nil, errors.New("host does not implement host.ContextBuilder")
	}
	return cb.ContextFor(ctx, queryText), nil
}

// Reply builds the window for queryText and streams one reply for it on the
// fast tier, both under ctx.
func Reply(ctx context.Context, h host.Host, queryText string) (string, error) {
	window, err := Window(ctx, h, queryText)
	if err != nil {
		return "", err
	}
	return Turn(ctx, h, window)
}
