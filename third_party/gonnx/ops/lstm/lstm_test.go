package lstm

import (
	"math/rand"
	"testing"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"github.com/stretchr/testify/assert"
	"gorgonia.org/tensor"
)

func TestLSTMInit(t *testing.T) {
	lstm := &LSTM{}
	err := lstm.Init(LSTMOnnxNodeProtoFixture())

	assert.Nil(t, err)
	assert.Equal(t, []float32{1.0}, lstm.activationAlpha)
	assert.Equal(t, []float32{2.0}, lstm.activationBeta)
	assert.Equal(t, []string{"sigmoid", "tanh", "relu"}, lstm.activations)
	assert.Equal(t, ops.Forward, lstm.direction)
	assert.Equal(t, 5, lstm.hiddenSize)
	assert.Equal(t, false, lstm.inputForget)
	assert.Equal(t, []string{"Y", "Y_h"}, lstm.outputs)
}

func TestLSTMInitUnkownAttr(t *testing.T) {
	lstm := LSTM{}
	tests := []struct {
		attr []*onnx.AttributeProto
		err  error
	}{
		{
			[]*onnx.AttributeProto{{Name: "clip"}},
			ops.ErrUnsupportedAttribute("clip", &lstm),
		},
		{
			[]*onnx.AttributeProto{{Name: "unknown"}},
			ops.ErrInvalidAttribute("unknown", &lstm),
		},
	}

	for _, test := range tests {
		err := lstm.Init(&onnx.NodeProto{Attribute: test.attr})
		assert.Equal(t, test.err, err)
	}
}

// layout=1 is the opset-14 batchwise layout, which LSTM does not implement. Its
// existing default: branch already rejects the unknown 'layout' attribute, so
// this test locks that behavior rather than adding code. layout=0 is the ONNX
// default (the attribute is omitted), which inits cleanly.
func TestLSTMInitRejectsLayout(t *testing.T) {
	lstm := &LSTM{}

	err := lstm.Init(&onnx.NodeProto{Attribute: []*onnx.AttributeProto{{Name: "layout", I: 1}}})
	assert.Equal(t, ops.ErrInvalidAttribute("layout", lstm), err)

	err = lstm.Init(&onnx.NodeProto{})
	assert.Nil(t, err)
}

func TestLSTM(t *testing.T) {
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
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh"), []byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
				Output: []string{"Y", "Y_h", "Y_c"},
			},
			lstmInput0,
			[]float32{0.9159305, 0.9356764, 0.87070554, 0.84180677},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh"), []byte("relu")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
				Output: []string{"Y", "Y_h", "Y_c"},
			},
			lstmInput0,
			[]float32{1.7530097, 1.7829735, 1.6231446, 1.5197954},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh"), []byte("relu")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
				Output: []string{"Y", "Y_h", "Y_c"},
			},
			lstmInput1,
			[]float32{10.598255, 10.547241, 10.214846, 10.267471},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh"), []byte("relu")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
				Output: []string{"Y", "Y_h", "Y_c"},
			},
			lstmInputNoBNoH,
			[]float32{8.276371, 8.291079, 8.161418, 7.7900877},
			nil,
		},
		{
			7,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "activation_alpha", Floats: []float32{}},
					{Name: "activation_beta", Floats: []float32{}},
					{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh"), []byte("tanh")}},
					{Name: "direction", S: []byte("forward")},
					{Name: "hidden_size", I: 4},
				},
				Output: []string{"Y", "Y_h", "Y_c"},
			},
			lstmInputPeepholes,
			[]float32{0.99891853, 0.99994266, 0.9995524, 0.99171203},
			nil,
		},
	}

	for _, test := range tests {
		inputs := test.inputs()

		lstm := lstmVersions[test.version]()
		err := lstm.Init(test.attrs)
		assert.Nil(t, err)

		res, err := lstm.Apply(inputs)
		assert.Equal(t, test.err, err)

		if err == nil {
			// Tolerance not exact equality: the stable Sigmoid (0.5*(1+tanh(x/2)))
			// shifts gate outputs by a last float32 ULP vs the old exp form (SOP-89).
			// 1e-5 (not 1e-6) because some cases reach magnitude ~8, where one float32
			// ULP is ~1e-6 and the accumulated shift is a couple of ULP.
			assert.InDeltaSlice(t, test.expected, res[1].Data(), 1e-5)
		}
	}
}

// lstmForwardAttrs builds the standard forward-LSTM attributes (sigmoid/tanh/tanh, hidden 4)
// with the given node output names. ONNX LSTM outputs are positional; the op must return the
// Y/Y_h/Y_c tensors by position regardless of the declared names.
func lstmForwardAttrs(outputs ...string) *onnx.NodeProto {
	return &onnx.NodeProto{
		Attribute: []*onnx.AttributeProto{
			{Name: "activation_alpha", Floats: []float32{}},
			{Name: "activation_beta", Floats: []float32{}},
			{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh"), []byte("tanh")}},
			{Name: "direction", S: []byte("forward")},
			{Name: "hidden_size", I: 4},
		},
		Output: outputs,
	}
}

// A node whose output names are NOT the canonical Y/Y_h/Y_c must still get its three outputs
// (currently they come back nil because the op keys an output map by canonical names).
func TestLSTMNonCanonicalOutputNames(t *testing.T) {
	lstm := lstmVersions[7]()
	err := lstm.Init(lstmForwardAttrs("my_Y", "my_Yh", "my_Yc"))
	assert.Nil(t, err)

	res, err := lstm.Apply(lstmInput0())
	assert.Nil(t, err)
	assert.Len(t, res, 3)
	for i := range res {
		assert.NotNilf(t, res[i], "output %d must not be nil", i)
	}
	// Y_h must equal the canonical-name run for the same config (first TestLSTM case).
	if res[1] != nil {
		assert.InDeltaSlice(t, []float32{0.9159305, 0.9356764, 0.87070554, 0.84180677}, res[1].Data(), 1e-6)
	}
}

// A node declaring a single (non-canonical) output gets exactly one non-nil tensor (the Y).
func TestLSTMSingleNonCanonicalOutput(t *testing.T) {
	lstm := lstmVersions[7]()
	err := lstm.Init(lstmForwardAttrs("only_output"))
	assert.Nil(t, err)

	res, err := lstm.Apply(lstmInput0())
	assert.Nil(t, err)
	assert.Len(t, res, 1)
	assert.NotNil(t, res[0])
}

// A node declaring two (non-canonical) outputs gets exactly [Y, Y_h], both non-nil.
func TestLSTMTwoNonCanonicalOutputs(t *testing.T) {
	lstm := lstmVersions[7]()
	err := lstm.Init(lstmForwardAttrs("out0", "out1"))
	assert.Nil(t, err)

	res, err := lstm.Apply(lstmInput0())
	assert.Nil(t, err)
	assert.Len(t, res, 2)
	for i := range res {
		assert.NotNilf(t, res[i], "output %d must not be nil", i)
	}
}

// Locks output ORDER and values: a non-canonical-name run must produce exactly the same
// Y/Y_h/Y_c tensors, in the same positions, as a canonical-name run. Guards against a fix
// that returns the outputs non-nil but mis-ordered (e.g. Y and Y_c swapped).
func TestLSTMOutputsMatchCanonicalRegardlessOfNames(t *testing.T) {
	canon := lstmVersions[7]()
	assert.Nil(t, canon.Init(lstmForwardAttrs("Y", "Y_h", "Y_c")))
	canonRes, err := canon.Apply(lstmInput0())
	assert.Nil(t, err)

	nonCanon := lstmVersions[7]()
	assert.Nil(t, nonCanon.Init(lstmForwardAttrs("a", "b", "c")))
	nonRes, err := nonCanon.Apply(lstmInput0())
	assert.Nil(t, err)

	assert.Len(t, nonRes, len(canonRes))
	for i := range canonRes {
		if assert.NotNilf(t, nonRes[i], "output %d must not be nil", i) {
			assert.Equalf(t, canonRes[i].Shape(), nonRes[i].Shape(), "output %d shape differs from canonical", i)
			assert.Equalf(t, canonRes[i].Data(), nonRes[i].Data(), "output %d differs from canonical", i)
		}
	}
}

// A malformed node declaring more than 3 outputs must not panic (the positional slice is
// capped at 3). ONNX LSTM has exactly 3 outputs, so this only guards against corrupt models.
func TestLSTMMoreThanThreeOutputsDoesNotPanic(t *testing.T) {
	lstm := lstmVersions[7]()
	assert.Nil(t, lstm.Init(lstmForwardAttrs("a", "b", "c", "d")))

	res, err := lstm.Apply(lstmInput0())
	assert.Nil(t, err)
	assert.Len(t, res, 3) // capped — never more than the 3 real outputs
}

func TestInputValidationLSTM(t *testing.T) {
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
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
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
				nil,
				nil,
			},
			nil,
		},
		{
			7,
			[]tensor.Tensor{ops.TensorWithBackingFixture([]float32{1, 2}, 2)},
			nil,
			ops.ErrInvalidOptionalInputCount(1, lstm7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(1, "int", lstm7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(0, "int", lstm7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(2, "int", lstm7BaseOpFixture()),
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
			ops.ErrInvalidInputType(3, "int", lstm7BaseOpFixture()),
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
			ops.ErrInvalidInputType(4, "float32", lstm7BaseOpFixture()),
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
			ops.ErrInvalidInputType(5, "int", lstm7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(6, "int", lstm7BaseOpFixture()),
		},
		{
			7,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
			},
			nil,
			ops.ErrInvalidInputType(7, "int", lstm7BaseOpFixture()),
		},
	}

	for _, test := range tests {
		lstm := lstmVersions[test.version]()
		validated, err := lstm.ValidateInputs(test.inputs)

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

func lstmInput0() []tensor.Tensor {
	r := rand.New(rand.NewSource(10))

	return []tensor.Tensor{
		// Input X: (sequence_length, batch_size, input_size).
		ops.RandomFloat32TensorFixture(r, 2, 1, 3),
		// Input W: (num_directions, 4 * hidden_size, input_size).
		ops.RandomFloat32TensorFixture(r, 1, 16, 3),
		// Input R: (num_directions, 4 * hidden_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 16, 4),
		// Input B: (num_directions, 8 * hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 32),
		// Input sequence_lens: not supported.
		nil,
		// Input initial_h: (num_directions, batch_size, hidden_size).
		ops.TensorWithBackingFixture(ops.Zeros(4), 1, 1, 4),
		// Input initial_c: (num_directions, batch_size, hidden_size).
		ops.TensorWithBackingFixture(ops.Zeros(4), 1, 1, 4),
		// Input P: peephole weights.
		nil,
	}
}

func lstmInput1() []tensor.Tensor {
	r := rand.New(rand.NewSource(11))

	return []tensor.Tensor{
		// Input X: (sequence_length, batch_size, input_size).
		ops.RandomFloat32TensorFixture(r, 10, 1, 3),
		// Input W: (num_directions, 4 * hidden_size, input_size).
		ops.RandomFloat32TensorFixture(r, 1, 16, 3),
		// Input R: (num_directions, 4 * hidden_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 16, 4),
		// Input B: (num_directions, 8 * hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 32),
		// Input sequence_lens: not supported.
		nil,
		// Input initial_h: (num_directions, batch_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 1, 4),
		// Input initial_c: (num_directions, batch_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 1, 4),
		// Input P: peephole weights.
		nil,
	}
}

func lstmInputNoBNoH() []tensor.Tensor {
	r := rand.New(rand.NewSource(12))

	return []tensor.Tensor{
		// Input X: (sequence_length, batch_size, input_size).
		ops.RandomFloat32TensorFixture(r, 10, 1, 3),
		// Input W: (num_directions, 4 * hidden_size, input_size).
		ops.RandomFloat32TensorFixture(r, 1, 16, 3),
		// Input R: (num_directions, 4 * hidden_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 16, 4),
		// Input B.
		nil,
		// Input sequence_lens: not supported.
		nil,
		// Input initial_h.
		nil,
		// Input initial_c.
		nil,
		// Input P: peephole weights.
		nil,
	}
}

func lstmInputPeepholes() []tensor.Tensor {
	r := rand.New(rand.NewSource(13))

	return []tensor.Tensor{
		// Input X: (sequence_length, batch_size, input_size).
		ops.RandomFloat32TensorFixture(r, 10, 1, 3),
		// Input W: (num_directions, 4 * hidden_size, input_size).
		ops.RandomFloat32TensorFixture(r, 1, 16, 3),
		// Input R: (num_directions, 4 * hidden_size, hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 16, 4),
		// Input B.
		nil,
		// Input sequence_lens: not supported.
		nil,
		// Input initial_h.
		nil,
		// Input initial_c.
		nil,
		// Input P: (num_directions, 3 * hidden_size).
		ops.RandomFloat32TensorFixture(r, 1, 12),
	}
}

func LSTMOnnxNodeProtoFixture() *onnx.NodeProto {
	return &onnx.NodeProto{
		Attribute: []*onnx.AttributeProto{
			{Name: "activation_alpha", Floats: []float32{1.0}},
			{Name: "activation_beta", Floats: []float32{2.0}},
			{Name: "activations", Strings: [][]byte{[]byte("sigmoid"), []byte("tanh"), []byte("relu")}},
			{Name: "direction", S: []byte("forward")},
			{Name: "hidden_size", I: 5},
			{Name: "input_forget", I: 0},
		},
		Output: []string{"Y", "Y_h"},
	}
}

func lstm7BaseOpFixture() ops.BaseOperator {
	return ops.NewBaseOperator(7, 3, 8, lstmTypeConstraints, "lstm")
}
