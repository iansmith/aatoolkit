package driver

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The queue must render clips one at a time, in enqueue order — so the ack
// finishes before the answer starts (no overlapping afplay).
func TestSpeechQueueSerial(t *testing.T) {
	var mu sync.Mutex
	var order []byte
	q := newSpeechQueue(func(text []byte, _ string, _ float64) {
		time.Sleep(time.Millisecond) // expose overlap if the queue isn't serial
		mu.Lock()
		order = append(order, text[0])
		mu.Unlock()
	})
	d1 := q.enqueue([]byte{1}, "", 1)
	d2 := q.enqueue([]byte{2}, "", 1)
	d3 := q.enqueue([]byte{3}, "", 1)
	<-d1
	<-d2
	<-d3
	if string(order) != string([]byte{1, 2, 3}) {
		t.Fatalf("play order = %v, want [1 2 3] (serial, in order)", order)
	}
}

// cancelQueued drains pending items without rendering them and closes their done
// channels so SpeakSync callers are not stranded.
func TestSpeechQueueCancelQueued(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var rendered []int

	q := newSpeechQueue(func(text []byte, _ string, _ float64) {
		if text[0] == 0 {
			close(started) // signal that clip 0 is rendering
			<-release      // block until the test says go
		} else {
			rendered = append(rendered, int(text[0]))
		}
	})

	// Clip 0 will block the worker while clips 1 and 2 queue up.
	q.enqueue([]byte{0}, "", 1)
	<-started // worker is now inside render for clip 0
	d1 := q.enqueue([]byte{1}, "", 1)
	d2 := q.enqueue([]byte{2}, "", 1)

	q.cancelQueued() // drop 1 and 2; close their done channels
	close(release)   // let clip 0 finish

	// done channels for cancelled clips must be closed (not block).
	mustClose := func(ch <-chan struct{}, name string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("%s not closed after cancelQueued", name)
		}
	}
	mustClose(d1, "d1")
	mustClose(d2, "d2")

	// Clips 1 and 2 must not have been rendered.
	if len(rendered) != 0 {
		t.Fatalf("expected no renders for cancelled clips, got %v", rendered)
	}
}

// Adversary: a single clip renders exactly once, and its done channel is
// close-only (not a value) and closed exactly once.
func TestSpeechQueueSingleClipAndCloseOnce(t *testing.T) {
	var n int32
	q := newSpeechQueue(func(text []byte, _ string, _ float64) { atomic.AddInt32(&n, 1) })
	done := q.enqueue([]byte{1}, "", 1)
	<-done
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("single clip: render called %d times, want 1", got)
	}
	select {
	case _, ok := <-done:
		if ok {
			t.Fatal("done channel delivered a value; want close-only")
		}
	default:
		t.Fatal("done channel not closed after the clip finished")
	}
}

// TestSynthesizeWAV_ReturnsBytes verifies that SynthesizeWAV fetches and returns
// WAV bytes from the TTS server without invoking afplay.
func TestSynthesizeWAV_ReturnsBytes(t *testing.T) {
	// Create a minimal valid WAV file for testing.
	wav := makeTestWAV([]int16{100, 200, 300})

	// Create a fake TTS server that returns the test WAV.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav)
	}))
	defer server.Close()

	// Create a Host with the fake TTS server.
	h := &Host{
		tiers:  make(map[string]Tier),
		client: &http.Client{},
		prompt: func() string { return "" },
		tts: TTSConfig{
			URL:    server.URL,
			Lang:   "en",
			Format: "wav",
		},
	}

	// Call SynthesizeWAV with a context.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	audio, err := h.SynthesizeWAV(ctx, []byte("test"), "alice", 1.0)
	if err != nil {
		t.Fatalf("SynthesizeWAV failed: %v", err)
	}

	if len(audio) == 0 {
		t.Fatal("SynthesizeWAV returned empty audio")
	}

	if string(audio) != string(wav) {
		t.Errorf("audio bytes mismatch: got %d bytes, want %d", len(audio), len(wav))
	}
}

// makeTestWAV creates a minimal valid WAV file containing the given samples.
func makeTestWAV(samples []int16) []byte {
	const (
		sampleRate     = 8000
		numChannels    = 1
		bitsPerSample  = 16
		bytesPerSample = bitsPerSample / 8
	)

	pcmLen := len(samples) * bytesPerSample
	wavLen := 44 + pcmLen

	wav := make([]byte, wavLen)

	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+pcmLen))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], numChannels)
	binary.LittleEndian.PutUint32(wav[24:28], sampleRate)
	binary.LittleEndian.PutUint32(wav[28:32], uint32(sampleRate*numChannels*bytesPerSample))
	binary.LittleEndian.PutUint16(wav[32:34], numChannels*bytesPerSample)
	binary.LittleEndian.PutUint16(wav[34:36], bitsPerSample)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(pcmLen))

	for i, sample := range samples {
		off := 44 + i*bytesPerSample
		binary.LittleEndian.PutUint16(wav[off:off+2], uint16(sample))
	}

	return wav
}
