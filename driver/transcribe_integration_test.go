package driver

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTranscribeAgainstLiveServer is a local smoke test of the whole path:
// the Go transcribe client → the FastAPI shim → mlx-whisper → a real transcript.
// It POSTs a real WAV and checks we get non-empty recognized speech back — a
// sanity check that we're "not totally wrong", not an assertion on exact words.
//
// It SKIPS (never fails) when the sample WAV is missing or the sidecar isn't
// running, so `go test ./...` stays green without the server. Run it with the
// sidecar up (aa-server-status: launch it, then run `voice-in up` at the prompt):
//
//	AATOOLKIT_STT_TEST_WAV=/tmp/accent3_off.wav \
//	  go test . -run TestTranscribeAgainstLiveServer -v
func TestTranscribeAgainstLiveServer(t *testing.T) {
	wav := EnvOr("AATOOLKIT_STT_TEST_WAV", "/tmp/accent3_off.wav")
	data, err := os.ReadFile(wav)
	if err != nil {
		t.Skipf("sample WAV %s not available: %v", wav, err)
	}

	endpoint := EnvOr("AATOOLKIT_STT_URL", "http://127.0.0.1:7789/v1/audio/transcriptions")
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		conn, derr := net.DialTimeout("tcp", u.Host, 500*time.Millisecond)
		if derr != nil {
			t.Skipf("whisper sidecar not reachable at %s (start via aa-server-status: `voice-in up`): %v", u.Host, derr)
		}
		conn.Close()
	}

	got, err := transcribe(&http.Client{}, endpoint, data)
	if err != nil {
		t.Fatalf("transcribe against live server: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("live transcript is empty for %s — expected some recognized speech", wav)
	}
	t.Logf("transcript of %s: %q", wav, got)
}
