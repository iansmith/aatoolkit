package gonnx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorgonia.org/tensor"
)

// sileroGoldens mirrors sample_models/silero_vad/goldens.json, produced by
// generate_goldens.py with onnxruntime. state_n stores the [2,1,128] recurrent
// state with the batch axis (1) elided as a 2x128 nested list.
type sileroGoldens struct {
	SampleRate int           `json:"sample_rate"`
	WindowSize int           `json:"window_size"`
	StateShape []int         `json:"state_shape"`
	Frames     []sileroFrame `json:"frames"`
}

type sileroFrame struct {
	Index  int         `json:"index"`
	Kind   string      `json:"kind"`
	Input  []float32   `json:"input"`
	Output float32     `json:"output"`
	StateN [][]float32 `json:"state_n"`
}

// TestSileroVADMatchesGoldens is the end-to-end acceptance test for the whole
// gonnx VAD effort: it loads the unmodified upstream silero_vad.onnx (opset 16,
// with If subgraphs, Pad and Size) and drives it frame-by-frame the way the engine
// will — 8 kHz, 256-sample float32 frames, sr=8000 as a rank-0 int64, a zero
// initial state, and each frame's stateN threaded into the next frame's state —
// then checks both the speech probability and the recurrent state against the
// onnxruntime goldens within the conformance delta of 1e-3.
func TestSileroVADMatchesGoldens(t *testing.T) {
	const delta = 1e-3

	modelPath := filepath.Join("sample_models", "onnx_models", "silero_vad.onnx")
	goldensPath := filepath.Join("sample_models", "silero_vad", "goldens.json")

	raw, err := os.ReadFile(goldensPath)
	require.NoErrorf(t, err, "reading goldens.json (the vendored file must exist -- this is a hard failure, not a skip)")

	var g sileroGoldens

	require.NoError(t, json.Unmarshal(raw, &g))
	require.GreaterOrEqual(t, len(g.Frames), 8, "goldens must cover at least 8 frames")
	require.Equal(t, []int{2, 1, 128}, g.StateShape, "unexpected state shape")

	var speech, silence int

	for _, f := range g.Frames {
		switch f.Kind {
		case "speech":
			speech++
		case "silence":
			silence++
		}
	}

	require.Positivef(t, speech, "goldens must include at least one speech frame")
	require.Positivef(t, silence, "goldens must include at least one near-silent frame")

	model, err := NewModelFromFile(modelPath)
	require.NoErrorf(t, err, "loading silero_vad.onnx (the vendored model must load -- hard failure, not a skip)")

	// Zero initial recurrent state [2,1,128]; sr is a rank-0 int64 scalar.
	stateElems := g.StateShape[0] * g.StateShape[1] * g.StateShape[2]

	var state tensor.Tensor = tensor.New(
		tensor.WithShape(g.StateShape...), tensor.WithBacking(make([]float32, stateElems)),
	)

	sr := tensor.New(tensor.FromScalar(int64(g.SampleRate)))

	for _, f := range g.Frames {
		input := tensor.New(
			tensor.WithShape(1, len(f.Input)),
			tensor.WithBacking(append([]float32(nil), f.Input...)),
		)

		outputs, err := model.Run(Tensors{"input": input, "state": state, "sr": sr})
		require.NoErrorf(t, err, "frame %d (%s): Run", f.Index, f.Kind)

		gotOut, ok := outputs["output"].Data().([]float32)
		require.Truef(t, ok && len(gotOut) == 1, "frame %d: output is not a single float32", f.Index)
		assert.InDeltaf(t, f.Output, gotOut[0], delta, "frame %d (%s): speech probability", f.Index, f.Kind)

		stateN := outputs["stateN"]

		want := make([]float32, 0, stateElems)
		for _, layer := range f.StateN {
			want = append(want, layer...)
		}

		assert.InDeltaSlicef(t, want, stateN.Data(), delta, "frame %d (%s): stateN", f.Index, f.Kind)

		// Thread the model's own new state into the next frame, so drift accumulates
		// and is caught at its source rather than only where it reaches the output.
		state = stateN
	}
}
