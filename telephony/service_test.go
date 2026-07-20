package telephony

import (
	"context"
	"math"
	"testing"
)

// TestDataPlaneBufferInPerceptualBand verifies that the configured
// DataPlaneBufferMS results in buffer depths within the perceptual band
// [50, 100) ms for standard frame sizes, and that other settings would fail.
func TestDataPlaneBufferInPerceptualBand(t *testing.T) {
	frameSizes := []int{10, 16, 20, 25, 32, 40}

	tests := []struct {
		name     string
		bufferMS int
		wantPass bool
	}{
		{"80ms", DataPlaneBufferMS, true},
		{"40ms", 40, false},
		{"120ms", 120, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allInBand := true

			for _, frameMS := range frameSizes {
				depth := int(math.Ceil(float64(tc.bufferMS) / float64(frameMS)))
				bufferDurationMS := depth * frameMS
				inBand := 50 < bufferDurationMS && bufferDurationMS <= 100

				if !inBand {
					allInBand = false
					break
				}
			}

			if tc.wantPass && !allInBand {
				t.Errorf("buffer %dms: expected all frame sizes in band [50,100)", tc.bufferMS)
			}
			if !tc.wantPass && allInBand {
				t.Errorf("buffer %dms: expected at least one frame size out of band", tc.bufferMS)
			}
		})
	}
}

// TestBufferedChan_SendRecv verifies that BufferedChan send/recv works correctly.
func TestBufferedChan_SendRecv(t *testing.T) {
	ch := NewBufferedChan[string](2)
	ctx := context.Background()

	err := ch.Send(ctx, "hello")
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	val, err := ch.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv failed: %v", err)
	}

	if val != "hello" {
		t.Errorf("expected hello, got %v", val)
	}

	underlyingChan := ch.Channel()
	if underlyingChan == nil {
		t.Error("Channel() returned nil")
	}
}

// TestBufferedChan_Depth verifies that BufferedChan wraps a channel
// with the correct depth capacity.
func TestBufferedChan_Depth(t *testing.T) {
	const bufferMS = DataPlaneBufferMS
	const frameMS = MuLawFrameMS

	expectedDepth := ComputeDepth(bufferMS, frameMS)
	ch := NewBufferedChan[[]byte](expectedDepth)
	underlyingChan := ch.Channel()

	if cap(underlyingChan) != expectedDepth {
		t.Errorf("expected capacity %d, got %d", expectedDepth, cap(underlyingChan))
	}
}
