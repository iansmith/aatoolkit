package gonnx

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorgonia.org/tensor"
)

// realAudioGoldens mirrors sample_models/silero_vad/realaudio_goldens.json,
// produced by generate_realaudio_goldens.py with onnxruntime over the committed
// test_10s.ulaw clip. It stores only the per-frame reference probability (plus rms
// and peak for diagnostics) -- the decoded input is regenerated in Go, byte-for-byte
// identical to audioop, by ulawToFloat32 below.
type realAudioGoldens struct {
	SampleRate int    `json:"sample_rate"`
	WindowSize int    `json:"window_size"`
	StateShape []int  `json:"state_shape"`
	Frames     []struct {
		Index  int     `json:"index"`
		Output float32 `json:"output"`
		RMS    float64 `json:"rms"`
		Peak   float64 `json:"peak"`
	} `json:"frames"`
}

// ulawToFloat32 decodes one G.711 mu-law byte to a float32 sample in [-1, 1),
// byte-identical to Python's audioop.ulaw2lin(raw, 2) followed by /32768. Verified
// exact (maxErr 0) against the audioop decode of test_10s.ulaw.
func ulawToFloat32(u byte) float32 {
	u = ^u
	sign := u & 0x80
	exp := (u >> 4) & 0x07
	man := u & 0x0F
	s := ((int(man) << 3) + 0x84) << exp
	s -= 0x84
	if sign != 0 {
		s = -s
	}
	return float32(int16(s)) / 32768.0
}

// TestSileroVADRealAudioNoNaN drives silero_vad.onnx over the full test_10s.ulaw
// clip (312 frames of real 8 kHz telephony audio), threading the recurrent state
// frame-to-frame exactly as the engine will. This is the SOP-89 regression: the 24-frame
// goldens never run long enough for the recurrent state to drift into the regime that
// tripped gorgonia's float32-exp overflow in Sigmoid, so the model produced NaN from
// frame 48 on. The assertion here is twofold: no output or state is ever non-finite,
// and every frame's probability tracks the onnxruntime reference.
func TestSileroVADRealAudioNoNaN(t *testing.T) {
	// Same 1e-3 conformance delta as the 24-frame goldens test. With the stable
	// Sigmoid, gonnx tracks onnxruntime to a whole-clip max abs diff of ~3e-6 across
	// all 312 threaded frames, so 1e-3 is a real constraint with ample headroom -- and
	// a regression that reintroduced the exp-overflow drift would blow past it long
	// before it NaNed.
	const realAudioDelta = 1e-3

	dir := filepath.Join("sample_models", "silero_vad")
	raw, err := os.ReadFile(filepath.Join(dir, "realaudio_goldens.json"))
	require.NoError(t, err, "reading realaudio_goldens.json (vendored -- hard failure, not a skip)")

	var g realAudioGoldens
	require.NoError(t, json.Unmarshal(raw, &g))
	require.GreaterOrEqual(t, len(g.Frames), 60, "real-audio goldens must cover >=60 frames to exercise state drift")
	require.Equal(t, []int{2, 1, 128}, g.StateShape)

	ulaw, err := os.ReadFile(filepath.Join(dir, "test_10s.ulaw"))
	require.NoError(t, err, "reading test_10s.ulaw")

	model, err := NewModelFromFile(filepath.Join("sample_models", "onnx_models", "silero_vad.onnx"))
	require.NoError(t, err)

	stateElems := g.StateShape[0] * g.StateShape[1] * g.StateShape[2]
	var state tensor.Tensor = tensor.New(
		tensor.WithShape(g.StateShape...), tensor.WithBacking(make([]float32, stateElems)),
	)
	sr := tensor.New(tensor.FromScalar(int64(g.SampleRate)))

	win := g.WindowSize
	// The goldens and the .ulaw are generated from the same clip, so this holds by
	// construction -- assert it up front so a desynced fixture fails legibly here
	// rather than as an opaque index-out-of-range panic mid-loop.
	lastFrame := g.Frames[len(g.Frames)-1].Index
	require.GreaterOrEqualf(t, len(ulaw), (lastFrame+1)*win,
		"test_10s.ulaw (%d bytes) is too short for %d frames of %d samples -- fixtures desynced", len(ulaw), lastFrame+1, win)

	for _, f := range g.Frames {
		samples := make([]float32, win)
		for i := range win {
			samples[i] = ulawToFloat32(ulaw[f.Index*win+i])
		}
		input := tensor.New(tensor.WithShape(1, win), tensor.WithBacking(samples))

		outputs, err := model.Run(Tensors{"input": input, "state": state, "sr": sr})
		require.NoErrorf(t, err, "frame %d: Run", f.Index)

		got, ok := outputs["output"].Data().([]float32)
		require.Truef(t, ok && len(got) == 1, "frame %d: output not a single float32", f.Index)
		require.Falsef(t, isNonFinite32(got[0]), "frame %d (rms=%.3f peak=%.3f): output is non-finite (%v)", f.Index, f.RMS, f.Peak, got[0])

		stateN := outputs["stateN"]
		for _, v := range stateN.Data().([]float32) {
			require.Falsef(t, isNonFinite32(v), "frame %d: recurrent state went non-finite", f.Index)
		}

		assert.InDeltaf(t, f.Output, got[0], realAudioDelta, "frame %d (rms=%.3f peak=%.3f): probability vs onnxruntime", f.Index, f.RMS, f.Peak)

		state = stateN
	}
}

func isNonFinite32(v float32) bool {
	f := float64(v)
	return math.IsNaN(f) || math.IsInf(f, 0)
}
