package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const defaultExtractTimeout = 30 * time.Second

// Client is an HTTP client for the extraction sidecar.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new extraction client.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: defaultExtractTimeout,
		},
	}
}

// Extract calls the extraction sidecar.
func (c *Client) Extract(ctx context.Context, req Request) ([]Span, error) {
	// Guard: empty text returns (nil, nil)
	if req.Text == "" {
		return nil, nil
	}

	// Guard: no labels returns error
	if len(req.Labels) == 0 {
		return nil, fmt.Errorf("extract: labels required")
	}

	// Guard: threshold out of range returns error
	if req.Threshold < 0 || req.Threshold > 1 {
		return nil, fmt.Errorf("extract: threshold must be between 0 and 1")
	}

	// Marshal request
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("extract: marshal request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+Path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("extract: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Execute request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("extract: calling extract sidecar at %s: %w", c.baseURL+Path, err)
	}
	defer resp.Body.Close()

	// Check status
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("extract sidecar returned %d", resp.StatusCode)
	}

	// Decode response
	var respBody struct {
		Spans []Span `json:"spans"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return nil, fmt.Errorf("extract: decode response: %w", err)
	}

	return respBody.Spans, nil
}

var _ Extractor = (*Client)(nil)
