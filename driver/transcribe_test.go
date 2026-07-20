package driver

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// recordingWhisper mimics a whisper transcription endpoint: it records whether it
// was hit and the audio bytes it received (from the multipart upload), and
// replies with a fixed status and body. This lets the tests assert both what
// transcribe returns and what it actually sent, with no real whisper server.
type recordingWhisper struct {
	srv      *httptest.Server
	hit      bool
	gotAudio []byte
}

func newRecordingWhisper(t *testing.T, status int, body string) *recordingWhisper {
	t.Helper()
	rw := &recordingWhisper{}
	rw.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw.hit = true
		// Pull the uploaded audio out of whatever multipart file field it arrives
		// in, so the test doesn't hard-code the field name.
		if err := r.ParseMultipartForm(1 << 20); err == nil && r.MultipartForm != nil {
			for _, fhs := range r.MultipartForm.File {
				if len(fhs) == 0 {
					continue
				}
				if f, err := fhs[0].Open(); err == nil {
					b, _ := io.ReadAll(f)
					f.Close()
					rw.gotAudio = b
				}
				break
			}
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(rw.srv.Close)
	return rw
}

// --- Edge / boundary cases -------------------------------------------------

// whisper routinely returns a leading space and/or trailing newline; the turn
// text fed to the policy must be trimmed the way typed input already is.
func TestTranscribeTrimsSurroundingWhitespace(t *testing.T) {
	rw := newRecordingWhisper(t, 200, `{"text":"  hello world\n"}`)
	got, err := transcribe(rw.srv.Client(), rw.srv.URL, []byte("RIFFfake"))
	if err != nil {
		t.Fatalf("transcribe: unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("transcript = %q, want %q (surrounding whitespace not trimmed)", got, "hello world")
	}
}

// Silence produces an empty transcript. That's a valid empty turn, not an error,
// and we must still have actually contacted the server (no local short-circuit).
func TestTranscribeEmptyTranscriptIsNotAnError(t *testing.T) {
	rw := newRecordingWhisper(t, 200, `{"text":""}`)
	got, err := transcribe(rw.srv.Client(), rw.srv.URL, []byte("RIFFsilence"))
	if err != nil {
		t.Fatalf("transcribe: unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("transcript = %q, want empty", got)
	}
	if !rw.hit {
		t.Fatal("whisper endpoint was never called for empty audio")
	}
}

// --- Error / rejection cases -----------------------------------------------

func TestTranscribeServerErrorReturnsError(t *testing.T) {
	rw := newRecordingWhisper(t, 500, `internal error`)
	if _, err := transcribe(rw.srv.Client(), rw.srv.URL, []byte("RIFFfake")); err == nil {
		t.Fatal("expected an error when the whisper server returns 500, got nil")
	}
}

func TestTranscribeMalformedJSONReturnsError(t *testing.T) {
	rw := newRecordingWhisper(t, 200, `this is not json`)
	if _, err := transcribe(rw.srv.Client(), rw.srv.URL, []byte("RIFFfake")); err == nil {
		t.Fatal("expected an error when the whisper response is not valid JSON, got nil")
	}
}

// Adversary gap: the whisper server being unreachable (down / wrong port) must
// surface as an error, not a silent empty transcript — mirrors how Send handles
// a failed client.Do. Own server so the single Close isn't doubled by cleanup.
func TestTranscribeTransportErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client, url := srv.Client(), srv.URL
	srv.Close() // nothing is listening at url anymore
	if _, err := transcribe(client, url, []byte("RIFFfake")); err == nil {
		t.Fatal("expected an error when the whisper server is unreachable, got nil")
	}
}

// Adversary gap: valid JSON with no "text" field (e.g. an empty object) is a
// distinct case from non-JSON — treat it as an empty transcript, not a crash or
// error.
func TestTranscribeMissingTextFieldIsEmpty(t *testing.T) {
	rw := newRecordingWhisper(t, 200, `{"segments":[]}`)
	got, err := transcribe(rw.srv.Client(), rw.srv.URL, []byte("RIFFfake"))
	if err != nil {
		t.Fatalf("transcribe: unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("transcript = %q, want empty for a response with no text field", got)
	}
	if !rw.hit {
		t.Fatal("whisper endpoint was never called")
	}
}

// --- Cross-feature: the request actually carries the audio -----------------

// A client that posted an empty or wrong body would still "work" against a canned
// server, so pin that the exact WAV bytes reach the endpoint.
func TestTranscribeUploadsTheAudioBytes(t *testing.T) {
	rw := newRecordingWhisper(t, 200, `{"text":"ok"}`)
	wav := []byte("RIFF....WAVEfmt  some pcm data")
	if _, err := transcribe(rw.srv.Client(), rw.srv.URL, wav); err != nil {
		t.Fatalf("transcribe: unexpected error: %v", err)
	}
	if !bytes.Equal(rw.gotAudio, wav) {
		t.Fatalf("server received audio %q, want %q", rw.gotAudio, wav)
	}
}

// --- Happy path ------------------------------------------------------------

func TestTranscribeReturnsText(t *testing.T) {
	rw := newRecordingWhisper(t, 200, `{"text":"hello world"}`)
	got, err := transcribe(rw.srv.Client(), rw.srv.URL, []byte("RIFFfake"))
	if err != nil {
		t.Fatalf("transcribe: unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("transcript = %q, want %q", got, "hello world")
	}
	if !rw.hit {
		t.Fatal("whisper endpoint was never called")
	}
}
