package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// transcribe posts a WAV to a whisper transcription endpoint (the local
// mlx-whisper FastAPI shim in scripts/whisper_server.py, which speaks the
// OpenAI-compatible multipart /v1/audio/transcriptions shape) and returns the
// recognized text, trimmed of the surrounding whitespace whisper tends to emit.
// Like Send and synthesizeAndPlay, the driver only does an HTTP POST here — the
// model runs in the separate sidecar process, so the driver stays cgo-free.
func transcribe(client *http.Client, url string, wav []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wav); err != nil {
		return "", err
	}
	// response_format=json keeps the reply a simple {"text": "..."} object.
	if err := mw.WriteField("response_format", "json"); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling whisper %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper server returned %d: %.200s", resp.StatusCode, raw)
	}

	var tr struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil {
		return "", fmt.Errorf("decoding whisper response: %w (raw: %.200s)", err, raw)
	}
	return strings.TrimSpace(tr.Text), nil
}
