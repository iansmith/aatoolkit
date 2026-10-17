// Package policy is an interpreted policy that builds its window under a
// caller's context, reaching the context-taking form of Context by asserting
// host.ContextBuilder on the host.Host it is handed.
package policy

import (
	"context"
	"errors"

	"github.com/iansmith/aatoolkit/host"
)

// Window returns the messages array for queryText, built under ctx.
func Window(ctx context.Context, h host.Host, queryText string) ([]byte, error) {
	cb, ok := h.(host.ContextBuilder)
	if !ok {
		return nil, errors.New("host does not implement host.ContextBuilder")
	}
	return cb.ContextFor(ctx, queryText), nil
}
