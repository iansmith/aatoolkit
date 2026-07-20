package factdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Extractor produces a structured Result for one user turn, given the prior turns
// and the entity handles already known, so the caller can extract incrementally.
type Extractor interface {
	Extract(ctx context.Context, priorTurns []Turn, known []Entity, utterance string) (*Result, error)
}

// LLMExtractor calls an OpenAI-compatible /v1/chat/completions endpoint. It is
// bound to an Ontology, which supplies the system prompt and the schema that
// constrain generation. Point URL/Model at whatever OpenAI-format server you run;
// a smaller "fast" model and a larger reasoning model can be compared by swapping
// them.
type LLMExtractor struct {
	URL       string // .../v1/chat/completions
	Model     string
	MaxTokens int
	Client    *http.Client
	Ontology  Ontology
}

// NewLLMExtractor builds an extractor for the given ontology against an
// OpenAI-format endpoint. Honors AATOOLKIT_FTEST_URL / AATOOLKIT_FTEST_MODEL
// overrides (e.g. to point at a different model on the same server); the defaults
// assume a local mlx-serve on port 1234 that speaks OpenAI format directly.
func NewLLMExtractor(ont Ontology) *LLMExtractor {
	return &LLMExtractor{
		URL:       envOr("AATOOLKIT_FTEST_URL", "http://127.0.0.1:1234/v1/chat/completions"),
		Model:     envOr("AATOOLKIT_FTEST_MODEL", "mlx-community/gemma-4-31b-it-8bit"),
		MaxTokens: 2048,
		Client:    &http.Client{Timeout: 120 * time.Second},
		Ontology:  ont,
	}
}

// Extract calls the model and parses its JSON reply into a Result.
func (x *LLMExtractor) Extract(ctx context.Context, priorTurns []Turn, known []Entity, utterance string) (*Result, error) {
	msgs, err := x.Ontology.BuildMessages(priorTurns, known, utterance)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model":       x.Model,
		"messages":    json.RawMessage(msgs),
		"temperature": 0.2, // low: extraction wants determinism, not creativity
		"stream":      false,
		"max_tokens":  x.MaxTokens,
		// gemma has no thinking mode; disabling it is a harmless no-op there and
		// keeps a reasoning model from burying the JSON behind a thinking block.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		// Constrain output to the extraction schema (predicate enum etc.) at
		// generation time — the mlx-serve gemma build advertises json_schema.
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "fact_extraction",
				"schema": x.Ontology.ResultSchema(),
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", x.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := x.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", x.URL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm %s: status %d (%.300s)", x.URL, resp.StatusCode, raw)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil {
		return nil, fmt.Errorf("decoding completion: %w", err)
	}
	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("no choices in completion")
	}
	return ParseResult(completion.Choices[0].Message.Content)
}

// ParseResult extracts the JSON object from a model reply and unmarshals it into a
// Result. It tolerates code fences and surrounding prose by slicing from the first
// '{' to the last '}' — small models often wrap JSON in chatter despite instructions.
func ParseResult(content string) (*Result, error) {
	s := strings.TrimSpace(content)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j >= i {
			s = s[i : j+1]
		}
	}
	var r Result
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, fmt.Errorf("parsing model JSON: %w\n---content---\n%s", err, content)
	}
	return &r, nil
}
