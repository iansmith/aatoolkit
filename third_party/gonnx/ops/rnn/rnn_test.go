package rnn

import (
	"math/rand"
	"testing"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorgonia.org/tensor"
)

func TestRNNInit(t *testing.T) {
	rnn := &RNN{}
	err := rnn.Init(RNNOnnxNodeProtoFixture())

	assert.Nil(t, err)
	assert.Equal(t, []float32{1.0}, rnn.activationAlpha)
	assert.Equal(t, []float32{2.0}, rnn.activationBeta)
	assert.Equal(t, []string{"sigmoid"}, rnn.activations)
	assert.Equal(t, ops.SequenceProcessDirection("forward"), rnn.direction)
	assert.Equal(t, 5, rnn.hiddenSize)
}

func TestRNNInitUnsupportedAttr(t *testing.T) {
	rnn := RNN{}
	err := rnn.Init(&onnx.NodeProto{Attribute: []*onnx.AttributeProto{{Name: "clip"}}})
	assert.Equal(t, err, ops.ErrUnsupportedAttribute("clip", &rnn))
}

func TestRNNInitUnknownAttr(t *testing.T) {
	rnn := RNN{}
	err := rnn.Init(&onnx.NodeProto{Attribute: []*onnx.AttributeProto{{Name: "unknown"}}})
	assert.Equal(t, err, ops.ErrInvalidAttribute("unknown", &rnn))
}

// layout=1 is the opset-14 batchwise layout, which RNN does not implement. Its
// existing default: branch already rejects the unknown 'layout' attribute, so
// this test locks that behavior rather than adding code. layout=0 is the ONNX
// default (the attribute is omitted), which inits cleanly.
func TestRNNInitRejectsLayout(t *testing.T) {
	rnn := &RNN{}

	err := rnn.Init(&onnx.NodeProto{Attribute: []*onnx.AttributeProto{{Name: "layout", I: 1}}})
	assert.Equal(t, ops.ErrInvalidAttribute("layout", rnn), err)

	err = rnn.Init(&onnx.NodeProto{})
	assert.Nil(t, err)
}

func TestRNN(t *testing.T) {
	tests := []struct {
		version  int64
		attrs    *onnx.NodeProto
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
					{Name: "activations", Strings: [][]byte{[]byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
			},
			rnnInput0,
			[]float32{0.78036773, 0.97858655, 0.94110376, 0.90722954},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
			},
			rnnInput0,
			[]float32{0.82048327, 0.922734, 0.89050114, 0.8620579},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("relu")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
			},

			rnnInput0,
			[]float32{1.0667435, 2.328037, 1.7986122, 1.545068},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 10},
				},
			},
			rnnInput1,
			[]float32{0.99996024, 0.9999855, 0.99998087, 0.9999288, 0.9997511, 0.99918234, 0.99999964, 0.9999981, 0.9997658, 0.9999618, 0.9998762, 0.9999353, 0.9999194, 0.9999428, 0.9997284, 0.9982606, 0.999999, 0.9999897, 0.99964744, 0.9998234, 0.99997497, 0.9999893, 0.9999906, 0.9999812, 0.99983937, 0.99967873, 0.9999998, 0.9999965, 0.9999516, 0.9999541},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
			},
			rnnInputNoB,
			// Same values as first test, but B is initialized automatically.
			[]float32{0.78036773, 0.97858655, 0.94110376, 0.90722954},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
			},
			rnnInputNoBNoH,
			// Same values as first test, but B and H are initialized automatically.
			[]float32{0.78036773, 0.97858655, 0.94110376, 0.90722954},
			nil,
		},
	}

	for _, test := range tests {
		inputs := test.inputs()

		rnn := rnnVersions[test.version]()
		err := rnn.Init(test.attrs)
		assert.Nil(t, err)

		res, err := rnn.Apply(inputs)
		assert.Equal(t, test.err, err)

		if err == nil {
			// Tolerance not exact equality: the stable Sigmoid (0.5*(1+tanh(x/2)))
			// shifts sigmoid-activated outputs by a last float32 ULP vs the old exp
			// form (SOP-89).
			assert.InDeltaSlice(t, test.expected, res[1].Data(), 1e-6)
		}
	}
}

// TestRNNDeclaredOutputCount is the SOP-92 regression: RNN's Apply used to
// hard-return two tensors, so a node declaring a different output count (ONNX
// RNN outputs are all optional) failed at the executor's len(names) ==
// len(outputTensors) check. Apply must now truncate its return to the declared
// count -- mirroring the LSTM fix in SOP-87.
func TestRNNDeclaredOutputCount(t *testing.T) {
	base := []*onnx.AttributeProto{
		{Name: "activations", Strings: [][]byte{[]byte("tanh")}},
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
			rnn := rnnVersions[7]()
			require.NoError(t, rnn.Init(&onnx.NodeProto{Output: tt.outputs, Attribute: base}))

			res, err := rnn.Apply(rnnInput0())
			require.NoError(t, err)
			assert.Len(t, res, tt.wantLen)
		})
	}
}

func TestInputValidationRNN(t *testing.T) {
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
			ops.ErrInvalidOptionalInputCount(1, rnn7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(0, "int", rnn7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(1, "int", rnn7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(2, "int", rnn7BaseOpFixture()),
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
			ops.ErrInvalidInputType(3, "int", rnn7BaseOpFixture()),
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
			ops.ErrInvalidInputType(4, "float32", rnn7BaseOpFixture()),
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
			ops.ErrInvalidInputType(5, "int", rnn7BaseOpFixture()),
		},
	}

	for _, test := range tests {
		rnn := rnnVersions[test.version]()
		validated, err := rnn.ValidateInputs(test.inputs)

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

func rnnInput0() []tensor.Tensor {
	r := rand.New(rand.NewSource(13))

	return []tensor.Tensor{
		// Input X: (sequence_length, batch_size, input_size).
		ops.RandomFloat32TensorFixture(r, 2, 1, 3),
		// Input W: (num_directions, hidden_size, input_size).
		ops.RandomFloat32TensorFixture(r, 1, 4, 3),
		// Input R: (num_directions, hidden_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 4, 4),
		// Input B: (num_directions, 2 * hidden_size)
		ops.TensorWithBackingFixture(ops.Zeros(ops.NElements(1, 8)), 1, 8),
		// Input sequence_lens: not supported
		nil,
		// Input initial_h: (num_directions, batch_size, hidden_size)
		ops.TensorWithBackingFixture(ops.Zeros(ops.NElements(1, 1, 4)), 1, 1, 4),
	}
}

func rnnInput1() []tensor.Tensor {
	r := rand.New(rand.NewSource(13))

	return []tensor.Tensor{
		// Input X: (sequence_length, batch_size, input_size).
		ops.RandomFloat32TensorFixture(r, 10, 3, 4),
		// Input W: (num_directions, hidden_size, input_size).
		ops.RandomFloat32TensorFixture(r, 1, 10, 4),
		// Input R: (num_directions, hidden_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 10, 10),
		// Input B: (num_directions, 2 * hidden_size)
		ops.TensorWithBackingFixture(ops.Zeros(ops.NElements(1, 20)), 1, 20),
		// Input sequence_lens: not supported
		nil,
		// Input initial_h: (num_directions, batch_size, hidden_size)
		ops.TensorWithBackingFixture(ops.Zeros(ops.NElements(1, 3, 10)), 1, 3, 10),
	}
}

func rnnInputNoB() []tensor.Tensor {
	r := rand.New(rand.NewSource(13))

	return []tensor.Tensor{
		// Input X: (sequence_length, batch_size, input_size).
		ops.RandomFloat32TensorFixture(r, 2, 1, 3),
		// Input W: (num_directions, hidden_size, input_size).
		ops.RandomFloat32TensorFixture(r, 1, 4, 3),
		// Input R: (num_directions, hidden_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 4, 4),
		// Input B: not provided.
		nil,
		// Input sequence_lens: not supported
		nil,
		// Input initial_h: (num_directions, batch_size, hidden_size)
		ops.TensorWithBackingFixture(ops.Zeros(ops.NElements(1, 1, 4)), 1, 1, 4),
	}
}

func rnnInputNoBNoH() []tensor.Tensor {
	r := rand.New(rand.NewSource(13))

	return []tensor.Tensor{
		// Input X: (sequence_length, batch_size, input_size).
		ops.RandomFloat32TensorFixture(r, 2, 1, 3),
		// Input W: (num_directions, hidden_size, input_size).
		ops.RandomFloat32TensorFixture(r, 1, 4, 3),
		// Input R: (num_directions, hidden_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 4, 4),
		// Input B: not provided.
		nil,
		// Input sequence_lens: not supported
		nil,
		// Input initial_h: (num_directions, batch_size, hidden_size)
		nil,
	}
}

func RNNOnnxNodeProtoFixture() *onnx.NodeProto {
	return &onnx.NodeProto{
		Attribute: []*onnx.AttributeProto{
			{Name: "activation_alpha", Floats: []float32{1.0}},
			{Name: "activation_beta", Floats: []float32{2.0}},
			{Name: "activations", Strings: [][]byte{[]byte("sigmoid")}},
			{Name: "direction", S: []byte("forward")},
			{Name: "hidden_size", I: 5},
		},
	}
}

func rnn7BaseOpFixture() ops.BaseOperator {
	return ops.NewBaseOperator(7, 3, 6, rnnTypeConstraints, "rnn")
}
