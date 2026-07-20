package gru

import (
	"fmt"
	"testing"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorgonia.org/tensor"
)

func TestGruInit(t *testing.T) {
	gru := GRU{}
	err := gru.Init(GRUOnnxNodeProtoFixture())

	assert.Nil(t, err)
	assert.Equal(t, []float32{1.0}, gru.activationAlpha)
	assert.Equal(t, []float32{2.0}, gru.activationBeta)
	assert.Equal(t, []string{"sigmoid", "tanh"}, gru.activations)
	assert.Equal(t, gru.direction, ops.Forward)
	assert.Equal(t, 5, gru.hiddenSize)
	assert.Equal(t, true, gru.linearBeforeReset)
}

func TestGruInitUnkownAttr(t *testing.T) {
	gru := GRU{}
	tests := []struct {
		attr []*onnx.AttributeProto
		err  error
	}{
		{
			[]*onnx.AttributeProto{{Name: "clip"}},
			ops.ErrUnsupportedAttribute("clip", &gru),
		},
		{
			[]*onnx.AttributeProto{{Name: "unknown"}},
			ops.ErrInvalidAttribute("unknown", &gru),
		},
	}

	for _, test := range tests {
		err := gru.Init(&onnx.NodeProto{Attribute: test.attr})
		assert.Equal(t, test.err, err)
	}
}

// layout=1 is the opset-14 batchwise layout, which GRU does not implement. Its
// existing default: branch already rejects the unknown 'layout' attribute, so
// this test locks that behavior rather than adding code. layout=0 is the ONNX
// default (the attribute is omitted), which inits cleanly.
func TestGRUInitRejectsLayout(t *testing.T) {
	gru := GRU{}

	err := gru.Init(&onnx.NodeProto{Attribute: []*onnx.AttributeProto{{Name: "layout", I: 1}}})
	assert.Equal(t, ops.ErrInvalidAttribute("layout", &gru), err)

	err = gru.Init(&onnx.NodeProto{})
	assert.Nil(t, err)
}

func TestGru(t *testing.T) {
	tests := []struct {
		version  int64
		node     *onnx.NodeProto
		inputs   ops.InputFixture
		expected []float32
		err      error
	}{
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
					{Name: "linear_before_reset", I: 1},
				},
			},
			gruInput0,
			[]float32{6.6936556e-03, 8.3446503e-07, 0.0000000e+00, 0.0000000e+00},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
					{Name: "linear_before_reset", I: 0},
				},
			},
			gruInput0,
			[]float32{6.6936556e-03, 8.3446503e-07, 0.0000000e+00, 0.0000000e+00},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
					{Name: "linear_before_reset", I: 0},
				},
			},
			gruInput1,
			[]float32{0.44905475, 0.4406946, 0.43368173, 0.42782417},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
					{Name: "linear_before_reset", I: 0},
				},
			},
			gruInputNoBNoH,
			[]float32{0.24553154, 0.24553154, 0.24553154, 0.24553154},
			nil,
		},
	}

	for _, test := range tests {
		inputs := test.inputs()

		gru := gruVersions[test.version]()
		err := gru.Init(test.node)
		assert.Nil(t, err)

		res, err := gru.Apply(inputs)
		assert.Equal(t, test.err, err)

		if err == nil {
			// Tolerance not exact equality: the stable Sigmoid (0.5*(1+tanh(x/2)))
			// shifts gate outputs by a last float32 ULP vs the old exp form (SOP-89).
			assert.InDeltaSlice(t, test.expected, res[1].Data(), 1e-6)
		}
	}
}

// TestGruSigmoidSaturationRegression pins the SOP-76 divergence. Before SOP-89,
// ops.Sigmoid computed 1/(1+exp(-x)) on gorgonia's float32 tensor.Exp, which
// overflows past arg ~88 and returned 1.0 for a strongly-negative update-gate
// pre-activation, so this GRU's hidden state came back {..., 0, 1} instead of the
// correct {..., 0, 0}. onnxruntime 1.27.0 gives Y_h = [6.6936594e-3, 7.7486072e-7,
// 0, 0] for these inputs (both linear_before_reset values). The 1e-3 tolerance is
// deliberately far tighter than the 1.0 error the exp-overflow bug produced, so a
// regression to the unstable sigmoid fails loudly, while still absorbing the benign
// last-ULP differences between gonnx and onnxruntime.
func TestGruSigmoidSaturationRegression(t *testing.T) {
	// onnxruntime reference for gruInput0 with activations [sigmoid, tanh].
	wantYh := []float32{6.6936594e-03, 7.7486072e-07, 0, 0}

	for _, linearBeforeReset := range []int64{1, 0} {
		// A subtest per variant so a failure (or FailNow from require) in one does
		// not prevent the other from running -- SOP-76 requires both to be checked.
		t.Run(fmt.Sprintf("linear_before_reset=%d", linearBeforeReset), func(t *testing.T) {
			node := &onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
					{Name: "linear_before_reset", I: linearBeforeReset},
				},
			}

			gru := gruVersions[7]()
			require.NoError(t, gru.Init(node))

			res, err := gru.Apply(gruInput0())
			require.NoError(t, err)

			assert.InDeltaSlicef(t, wantYh, res[1].Data(), 1e-3,
				"GRU Y_h diverged from the onnxruntime reference (linear_before_reset=%d) -- "+
					"a value near 1.0 in the last element means the exp-overflow Sigmoid regressed (SOP-76/SOP-89)",
				linearBeforeReset)
		})
	}
}

// TestGruDeclaredOutputCount is the SOP-92 regression: GRU's Apply used to
// hard-return two tensors, so a node declaring a different output count (ONNX
// GRU outputs are all optional) failed at the executor's len(names) ==
// len(outputTensors) check. Apply must now truncate its return to the declared
// count -- mirroring the LSTM fix in SOP-87.
func TestGruDeclaredOutputCount(t *testing.T) {
	base := []*onnx.AttributeProto{
		{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh")}},
		{Name: "hidden_size", I: 4},
	}

	tests := []struct {
		name    string
		outputs []string
		wantLen int
	}{
		{"single Y", []string{"Y"}, 1},
		// A node that wants only Y_h encodes the absent Y as an empty string
		// (ONNX optional-output convention), so Output has length 2 and both
		// tensors come back -- the executor binds "" to Y (discarding it) and
		// "Y_h" to Yh positionally.
		{"Y_h only via empty Y slot", []string{"", "Y_h"}, 2},
		{"both", []string{"Y", "Y_h"}, 2},
		{"unspecified defaults to both", nil, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gru := gruVersions[7]()
			require.NoError(t, gru.Init(&onnx.NodeProto{Output: tt.outputs, Attribute: base}))

			res, err := gru.Apply(gruInput0())
			require.NoError(t, err)
			assert.Len(t, res, tt.wantLen)
		})
	}
}

func TestInputValidationGRU(t *testing.T) {
	tests := []struct {
		version  int64
		inputs   []tensor.Tensor
		expected []tensor.Tensor
		err      error
	}{
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			nil,
			nil,
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				nil,
				nil,
				nil,
			},
			nil,
		},
		{
			7,
			[]tensor.Tensor{ops.TensorWithBackingFixture([]float32{1, 2}, 2)},
			nil,
			ops.ErrInvalidOptionalInputCount(1, gru7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(1, "int", gru7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(0, "int", gru7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(1, "int", gru7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(2, "int", gru7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(3, "int", gru7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(4, "float32", gru7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(5, "int", gru7BaseOpFixture()),
		},
	}

	for _, test := range tests {
		gru := gruVersions[test.version]()
		validated, err := gru.ValidateInputs(test.inputs)

		assert.Equal(t, test.err, err)

		if test.err == nil {
			if test.expected != nil {
				assert.Equal(t, test.expected, validated)
			} else {
				assert.Equal(t, test.inputs, validated)
			}
		}
	}
}

func gruInput0() []tensor.Tensor {
	shapes := [][]int{{2, 1, 3}, {1, 12, 3}, {1, 12, 4}, {1, 24}, {1, 1, 4}}
	inputs := []tensor.Tensor{
		ops.Float32TensorFixture(shapes[0]...),
		ops.Float32TensorFixture(shapes[1]...),
		ops.Float32TensorFixture(shapes[2]...),
		ops.TensorWithBackingFixture(ops.Zeros(ops.NElements(shapes[3]...)), shapes[3]...),
		nil,
		ops.TensorWithBackingFixture(ops.Zeros(ops.NElements(shapes[4]...)), shapes[4]...),
	}

	return inputs
}

func gruInput1() []tensor.Tensor {
	shps := [][]int{{10, 1, 3}, {1, 12, 3}, {1, 12, 4}, {1, 24}, {1, 1, 4}}
	inputs := []tensor.Tensor{
		ops.Float32TensorFixture(shps[0]...),
		ops.TensorWithBackingFixture(ops.Full(ops.NElements(shps[1]...), 0.2), shps[1]...),
		ops.TensorWithBackingFixture(ops.Full(ops.NElements(shps[2]...), 0.5), shps[2]...),
		ops.TensorWithBackingFixture(ops.Arange(ops.NElements(shps[3]...), 0.10), shps[3]...),
		nil,
		ops.TensorWithBackingFixture(ops.Full(ops.NElements(shps[4]...), 0.4), shps[4]...),
	}

	return inputs
}

func gruInputNoBNoH() []tensor.Tensor {
	shps := [][]int{{10, 1, 3}, {1, 12, 3}, {1, 12, 4}, {1, 24}, {1, 1, 4}}
	inputs := []tensor.Tensor{
		ops.Float32TensorFixture(shps[0]...),
		ops.TensorWithBackingFixture(ops.Full(ops.NElements(shps[1]...), 0.2), shps[1]...),
		ops.TensorWithBackingFixture(ops.Full(ops.NElements(shps[2]...), 0.5), shps[2]...),
		nil,
		nil,
		nil,
	}

	return inputs
}

func GRUOnnxNodeProtoFixture() *onnx.NodeProto {
	return &onnx.NodeProto{
		Attribute: []*onnx.AttributeProto{
			{Name: "activation_alpha", Floats: []float32{1.0}},
			{Name: "activation_beta", Floats: []float32{2.0}},
			{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh")}},
			{Name: "direction", S: []byte("forward")},
			{Name: "hidden_size", I: 5},
			{Name: "linear_before_reset", I: 1},
		},
	}
}

func gru7BaseOpFixture() ops.BaseOperator {
	return ops.NewBaseOperator(7, 3, 6, gruTypeConstraints, "gru")
}
