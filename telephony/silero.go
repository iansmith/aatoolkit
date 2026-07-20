package telephony

import (
	"fmt"

	gonnx "github.com/advancedclimatesystems/gonnx"
	"gorgonia.org/tensor"

	"github.com/iansmith/aatoolkit/telephony/assets"
)

// sileroWindowSize is the Silero VAD v6.2.1 model's fixed inference window:
// 256 float32 PCM samples (32ms @ 8kHz).
const sileroWindowSize = 256

// sileroSampleRate is the sample rate the engine feeds the model — Twilio's
// μ-law audio, decoded to float32 PCM (see vad.go's decodeMuLaw).
const sileroSampleRate = 8000

// sileroStateShape is the model's recurrent state tensor shape: 2 LSTM
// layers, batch size 1, hidden size 128.
var sileroStateShape = []int{2, 1, 128}

// sileroStateElems is the flattened length of the recurrent state tensor —
// one definition shared by the in-process detector's Reset and the HTTP
// detector's wire encoding (silero_http.go), so the two can't drift.
func sileroStateElems() int {
	return sileroStateShape[0] * sileroStateShape[1] * sileroStateShape[2]
}

// sileroDetector implements vadDetector against the embedded Silero VAD ONNX
// model via gonnx. It is not safe for concurrent use: each session owns its
// own instance and its own recurrent state (SOP-93 owns any future pooling).
type sileroDetector struct {
	model *gonnx.Model
	state tensor.Tensor
	sr    tensor.Tensor
}

// NewSileroDetector builds a sileroDetector from the embedded, go:embed-ded
// model — no runtime file reads. It is the production vadFactory used by
// Session.Start (see WithVADFactory to override it in tests).
func NewSileroDetector() (vadDetector, error) {
	return newSileroDetectorFromBytes(assets.SileroVADONNX)
}

// newSileroDetectorFromBytes builds a sileroDetector from arbitrary ONNX
// model bytes. Split out from NewSileroDetector so ValidateVAD's failure path
// can be exercised against corrupt bytes without touching the real embedded
// asset (see validate_test.go).
func newSileroDetectorFromBytes(modelBytes []byte) (vadDetector, error) {
	model, err := gonnx.NewModelFromBytes(modelBytes)
	if err != nil {
		return nil, fmt.Errorf("silero: loading model: %w", err)
	}
	d := &sileroDetector{model: model}
	d.Reset()
	return d, nil
}

// Detect runs one inference window through the model, threading the
// recurrent state from the previous call. It rejects any window whose length
// isn't exactly the model's fixed window size.
func (d *sileroDetector) Detect(window []float32) (float32, error) {
	if len(window) != sileroWindowSize {
		return 0, fmt.Errorf("silero: window length %d != %d", len(window), sileroWindowSize)
	}

	input := tensor.New(
		tensor.WithShape(1, len(window)),
		tensor.WithBacking(append([]float32(nil), window...)),
	)

	outputs, err := d.model.Run(gonnx.Tensors{"input": input, "state": d.state, "sr": d.sr})
	if err != nil {
		return 0, fmt.Errorf("silero: inference: %w", err)
	}

	out, ok := outputs["output"].Data().([]float32)
	if !ok || len(out) != 1 {
		return 0, fmt.Errorf("silero: unexpected output shape from model")
	}

	stateN, ok := outputs["stateN"]
	if !ok {
		return 0, fmt.Errorf("silero: model did not return updated state")
	}
	d.state = stateN

	return out[0], nil
}

// Reset zeroes the recurrent state, starting a fresh utterance's inference
// history — called by runVAD on every exit (ctx cancel, closed input, or
// cancel mid-send) so a session's next use starts clean.
func (d *sileroDetector) Reset() {
	d.state = tensor.New(
		tensor.WithShape(sileroStateShape...),
		tensor.WithBacking(make([]float32, sileroStateElems())),
	)
	d.sr = tensor.New(tensor.FromScalar(int64(sileroSampleRate)))
}
