// Package extract provides the wire contract and interface for an extraction sidecar.
package extract

import "context"

const Path = "/extract"

// Request is the wire format for an extraction request.
type Request struct {
	Text      string   `json:"text"`
	Labels    []string `json:"labels"`
	Threshold float64  `json:"threshold"`
}

// Span is a single extracted entity.
type Span struct {
	Text  string  `json:"text"`
	Label string  `json:"label"`
	Start int     `json:"start"`
	End   int     `json:"end"`
	Score float64 `json:"score"`
}

// Extractor can extract entities from text.
type Extractor interface {
	Extract(ctx context.Context, req Request) ([]Span, error)
}

var _ Extractor = (*Client)(nil)
