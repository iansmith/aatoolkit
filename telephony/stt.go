package telephony

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const (
	// sttSampleRate is Twilio's μ-law rate: 8000 bytes = 1 second of audio.
	sttSampleRate = 8000

	// maxClipBytes is 30s of μ-law audio. Whisper ingests a 30s window, so a
	// longer clip is split before it reaches the sidecar.
	maxClipBytes = 30 * sttSampleRate

	// defaultSTTTimeout bounds a single transcription request. Whisper large-v3
	// has a ~1s floor per pass; this is a generous ceiling that still guarantees
	// a wedged sidecar cannot pin a live call's goroutine indefinitely.
	defaultSTTTimeout = 30 * time.Second
)

// STTPassKind identifies which recognition pass a request or result belongs
// to. FullPass is the sole remaining pass: a transcription over the full
// utterance.
type STTPassKind string

const (
	FullPass STTPassKind = "full"
)

// STTSegment is one segment of a whisper verbose_json transcription: a
// timed span of text with the model's confidence for that span.
type STTSegment struct {
	Text         string
	Start        float64
	End          float64
	AvgLogprob   float64
	NoSpeechProb float64
}

// STTRequest is the unit of work sent to sttService: a μ-law clip tagged
// with the session/request identity it must be correlated back to.
type STTRequest struct {
	SessionID string
	RequestID int
	Kind      STTPassKind
	Audio     []byte
}

// STTResult is the lossless whisper verbose_json record for one STTRequest,
// tagged with the same correlation identity the request carried (Charter R9).
type STTResult struct {
	SessionID string
	RequestID int
	Kind      STTPassKind
	Text      string
	Language  string
	Duration  float64
	Segments  []STTSegment
}

// STTInput is the receive side of the channel sttService reads STTRequests
// from (the SOP-116 ServiceInput[T] pattern).
type STTInput = ServiceInput[STTRequest]

// STTOutput is the send side of the channel sttService writes STTResults to
// (the SOP-116 ServiceOutput[T] pattern).
type STTOutput = ServiceOutput[STTResult]

// sttAbandon identifies a request to be abandoned before transcription.
type sttAbandon struct {
	SessionID string
	RequestID int
}

// sttService wraps STTClient.Transcribe with the channel-per-service
// pattern: it reads an STTRequest, transcribes its audio, and writes an
// STTResult carrying the request's correlation fields. Transcribe itself
// never sees SessionID/RequestID/Kind -- only sttService copies them.
type sttService struct {
	client     *STTClient
	input      STTInput
	output     STTOutput
	CancelChan chan sttAbandon
}

// NewSTTService returns an sttService reading from input and writing to output.
// The returned service has a cancel channel (CancelChan) that callers can use
// to mark requests as abandoned before transcription.
func NewSTTService(client *STTClient, input STTInput, output STTOutput) *sttService {
	return &sttService{
		client:     client,
		input:      input,
		output:     output,
		CancelChan: make(chan sttAbandon, 100),
	}
}

// newSTTService is an internal alias for NewSTTService used by tests.
func newSTTService(client *STTClient, input STTInput, output STTOutput) *sttService {
	return NewSTTService(client, input, output)
}

// maxAbandoned bounds the abandoned-set: a cancel for a request already
// in-flight, or one whose call ends before it's ever dequeued, is never
// removed by the dequeue-side delete in run(). Without a cap that leaves a
// permanent entry per such cancel, growing unboundedly over a long-running
// server's lifetime. Oldest entries are evicted first once the cap is hit.
const maxAbandoned = 1024

// run reads requests from the input channel and writes correlated results to
// the output channel until ctx is cancelled. It drains the cancel channel at
// loop boundaries to build an abandoned-set of requests that should be skipped.
func (s *sttService) run(ctx context.Context) {
	abandoned := make(map[sttAbandon]bool)
	var order []sttAbandon

	for {
		// Drain any pending cancels into the abandoned set
	drainCancels:
		for {
			select {
			case cancel := <-s.CancelChan:
				if !abandoned[cancel] {
					abandoned[cancel] = true
					order = append(order, cancel)
					if len(order) > maxAbandoned {
						delete(abandoned, order[0])
						order = order[1:]
					}
				}
			default:
				break drainCancels
			}
		}

		req, err := s.input.Recv(ctx)
		if err != nil {
			return
		}

		// Skip if this request was abandoned
		key := sttAbandon{SessionID: req.SessionID, RequestID: req.RequestID}
		if abandoned[key] {
			delete(abandoned, key)
			continue
		}

		result, err := s.client.Transcribe(ctx, fmt.Sprint(req.RequestID), req.Audio)
		if err != nil {
			// Prefix matches every other per-call line ("telephony: session
			// <SID>: ..."), so one call's whole timeline -- including the
			// failures, which is when you most need it -- greps out together.
			log.Printf("telephony: session %s: stt request %d (%s pass): transcribe failed: %v",
				req.SessionID, req.RequestID, req.Kind, err)
			continue
		}

		result.SessionID = req.SessionID
		result.RequestID = req.RequestID
		result.Kind = req.Kind

		if err := s.output.Send(ctx, result); err != nil {
			return
		}
	}
}

// STTClient transcribes μ-law telephony audio via an OpenAI-shaped
// /v1/audio/transcriptions endpoint (the whisper sidecar). It holds one
// keep-alive http.Client so chunked clips reuse a single connection.
type STTClient struct {
	url    string
	client *http.Client
}

// NewSTTClient returns a client posting to baseURL (e.g. http://127.0.0.1:7789).
func NewSTTClient(baseURL string) *STTClient {
	return &STTClient{
		url: strings.TrimSuffix(baseURL, "/"),
		client: &http.Client{
			Timeout: defaultSTTTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
			},
		},
	}
}

// Transcribe returns the full verbose_json decode of a μ-law clip. Clips
// longer than 30s are split into <=30s chunks transcribed in order; their
// text is joined and the first chunk's Language/Duration/Segments are kept
// (whisper's chunk-level detail beyond joined text is not defined across a
// multi-chunk clip -- callers needing per-chunk segments should not send
// clips over maxClipBytes).
func (c *STTClient) Transcribe(ctx context.Context, id string, mulawBytes []byte) (STTResult, error) {
	if len(mulawBytes) == 0 {
		return STTResult{}, nil
	}

	if len(mulawBytes) <= maxClipBytes {
		return c.transcribeChunk(ctx, id, mulawBytes)
	}

	var parts []string
	var combined STTResult
	for i := 0; i < len(mulawBytes); i += maxClipBytes {
		end := min(i+maxClipBytes, len(mulawBytes))

		result, err := c.transcribeChunk(ctx, id, mulawBytes[i:end])
		if err != nil {
			return STTResult{}, fmt.Errorf("chunk at %ds: %w", i/sttSampleRate, err)
		}
		if i == 0 {
			combined = result
		}
		parts = append(parts, result.Text)
	}

	combined.Text = strings.Join(parts, " ")
	return combined, nil
}

func (c *STTClient) transcribeChunk(ctx context.Context, id string, mulawBytes []byte) (STTResult, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		return STTResult{}, err
	}
	if _, err := part.Write(mulawToWAV(mulawBytes, sttSampleRate)); err != nil {
		return STTResult{}, err
	}
	if err := writer.WriteField("model", "whisper-1"); err != nil {
		return STTResult{}, err
	}
	if err := writer.WriteField("response_format", "verbose_json"); err != nil {
		return STTResult{}, err
	}
	if id != "" {
		if err := writer.WriteField("request_id", id); err != nil {
			return STTResult{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return STTResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/v1/audio/transcriptions", body)
	if err != nil {
		return STTResult{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return STTResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return STTResult{}, fmt.Errorf("stt: %s: %s", resp.Status, bytes.TrimSpace(respBody))
	}

	var decoded struct {
		Text      string  `json:"text"`
		Language  string  `json:"language"`
		Duration  float64 `json:"duration"`
		RequestId string  `json:"request_id"`
		Segments  []struct {
			Text         string  `json:"text"`
			Start        float64 `json:"start"`
			End          float64 `json:"end"`
			AvgLogprob   float64 `json:"avg_logprob"`
			NoSpeechProb float64 `json:"no_speech_prob"`
		} `json:"segments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return STTResult{}, fmt.Errorf("stt: decode response: %w", err)
	}

	if id != "" && decoded.RequestId != id {
		return STTResult{}, fmt.Errorf("stt: id mismatch: sent %q, got %q", id, decoded.RequestId)
	}

	// Decode stops at the end of the JSON value; net/http only returns a
	// connection to the idle pool once its body is read to EOF. Without this
	// drain every chunk would open a fresh connection despite the pool above.
	_, _ = io.Copy(io.Discard, resp.Body)

	segments := make([]STTSegment, len(decoded.Segments))
	for i, s := range decoded.Segments {
		segments[i] = STTSegment{
			Text:         s.Text,
			Start:        s.Start,
			End:          s.End,
			AvgLogprob:   s.AvgLogprob,
			NoSpeechProb: s.NoSpeechProb,
		}
	}

	return STTResult{
		Text:     decoded.Text,
		Language: decoded.Language,
		Duration: decoded.Duration,
		Segments: segments,
	}, nil
}
