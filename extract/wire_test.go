package extract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadFixture(t *testing.T) map[string]interface{} {
	fixtureDir := filepath.Join("testdata", "extract_wire_fixture.json")
	data, err := os.ReadFile(fixtureDir)
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}

	var fixture map[string]interface{}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("failed to unmarshal fixture: %v", err)
	}
	return fixture
}

func TestWireFixture_RequestEncodesByteIdentically(t *testing.T) {
	fixture := loadFixture(t)

	// Extract the request object from fixture
	requestData := fixture["request"].(map[string]interface{})
	labels := make([]string, 0)
	for _, l := range requestData["labels"].([]interface{}) {
		labels = append(labels, l.(string))
	}

	req := Request{
		Text:      requestData["text"].(string),
		Labels:    labels,
		Threshold: requestData["threshold"].(float64),
	}

	// Marshal and compare to fixture's request_json
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	expected := fixture["request_json"].(string)
	if string(encoded) != expected {
		t.Errorf("request encoding mismatch\nGot:      %s\nExpected: %s", string(encoded), expected)
	}
}

func TestWireFixture_ResponseDecodes(t *testing.T) {
	fixture := loadFixture(t)

	responseJSON := fixture["response_json"].(string)
	var resp struct {
		Spans []Span `json:"spans"`
	}
	if err := json.Unmarshal([]byte(responseJSON), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(resp.Spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(resp.Spans))
	}

	// Check first span
	if resp.Spans[0].Text != "Bill" {
		t.Errorf("span[0].Text = %q, want %q", resp.Spans[0].Text, "Bill")
	}
	if resp.Spans[0].Label != "birthday_person" {
		t.Errorf("span[0].Label = %q, want %q", resp.Spans[0].Label, "birthday_person")
	}
	if resp.Spans[0].Start != 0 {
		t.Errorf("span[0].Start = %d, want %d", resp.Spans[0].Start, 0)
	}
	if resp.Spans[0].End != 4 {
		t.Errorf("span[0].End = %d, want %d", resp.Spans[0].End, 4)
	}
	if resp.Spans[0].Score != 0.87 {
		t.Errorf("span[0].Score = %v, want %v", resp.Spans[0].Score, 0.87)
	}

	// Check second span
	if resp.Spans[1].Text != "5th of July" {
		t.Errorf("span[1].Text = %q, want %q", resp.Spans[1].Text, "5th of July")
	}
	if resp.Spans[1].Label != "birthday_date" {
		t.Errorf("span[1].Label = %q, want %q", resp.Spans[1].Label, "birthday_date")
	}
	if resp.Spans[1].Start != 12 {
		t.Errorf("span[1].Start = %d, want %d", resp.Spans[1].Start, 12)
	}
	if resp.Spans[1].End != 23 {
		t.Errorf("span[1].End = %d, want %d", resp.Spans[1].End, 23)
	}
	if resp.Spans[1].Score != 0.81 {
		t.Errorf("span[1].Score = %v, want %v", resp.Spans[1].Score, 0.81)
	}
}

func TestWireFixture_OffsetsAreUTF8Bytes(t *testing.T) {
	fixture := loadFixture(t)

	cases := fixture["byte_offset_cases"].([]interface{})
	if len(cases) < 3 {
		t.Fatalf("expected at least 3 byte_offset_cases, got %d", len(cases))
	}

	for i, caseData := range cases {
		caseMap := caseData.(map[string]interface{})
		text := caseMap["text"].(string)
		spanText := caseMap["span_text"].(string)
		byteStart := int(caseMap["byte_start"].(float64))
		byteEnd := int(caseMap["byte_end"].(float64))

		extracted := text[byteStart:byteEnd]
		if extracted != spanText {
			t.Errorf("case %d: text[%d:%d] = %q, want %q", i, byteStart, byteEnd, extracted, spanText)
		}

		// Also verify the stored values match expected
		expectedByteStart := int(caseMap["byte_start"].(float64))
		expectedByteEnd := int(caseMap["byte_end"].(float64))
		if byteStart != expectedByteStart {
			t.Errorf("case %d: byteStart = %d, want %d", i, byteStart, expectedByteStart)
		}
		if byteEnd != expectedByteEnd {
			t.Errorf("case %d: byteEnd = %d, want %d", i, byteEnd, expectedByteEnd)
		}
	}
}
