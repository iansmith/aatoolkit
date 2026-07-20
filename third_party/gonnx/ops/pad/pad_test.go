package pad

import (
	"testing"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorgonia.org/tensor"
)

// modeNode builds a NodeProto carrying only a `mode` attribute. A nil mode means
// the attribute is absent, which ONNX defines as mode="constant".
func modeNode(mode string) *onnx.NodeProto {
	if mode == "" {
		return &onnx.NodeProto{}
	}

	return &onnx.NodeProto{
		Attribute: []*onnx.AttributeProto{{Name: "mode", S: []byte(mode)}},
	}
}

// padsTensor builds the int64 `pads` input: [begin_0..begin_n, end_0..end_n].
func padsTensor(pads ...int64) tensor.Tensor {
	return ops.TensorWithBackingFixture(pads, len(pads))
}

func TestPadInit(t *testing.T) {
	tests := []struct {
		mode string
		err  error
	}{
		{"", nil}, // absent attribute => constant
		{"constant", nil},
		{"reflect", nil},
		{"edge", nil},
	}

	for _, test := range tests {
		p := &Pad{}
		require.NoError(t, p.Init(modeNode(test.mode)), "mode %q", test.mode)
	}
}

// wrap is opset-19 and axes is opset-18; both are out of scope and must fail
// loudly rather than silently computing something plausible.
func TestPadUnsupportedModeWrap(t *testing.T) {
	p := padVersions[13]()

	err := p.Init(modeNode("wrap"))

	require.Error(t, err, "mode=wrap must be rejected at Init")
	assert.Equal(t, ops.ErrUnsupportedAttribute("mode", p), err)
}

func TestPadUnknownModeRejected(t *testing.T) {
	p := padVersions[13]()

	err := p.Init(modeNode("nonsense"))

	require.Error(t, err, "an unknown mode must be rejected at Init")
}

// A 4th input is the opset-18 `axes`, which this operator does not implement.
// maxInputs=3 means ValidateInputs rejects it.
func TestPadAxesInputRejected(t *testing.T) {
	p := padVersions[13]()

	inputs := []tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4}, 2, 2),
		padsTensor(0, 0, 0, 0),
		ops.TensorWithBackingFixture([]float32{0}, 1),
		ops.TensorWithBackingFixture([]int64{1}, 1), // axes
	}

	_, err := p.ValidateInputs(inputs)

	require.Error(t, err, "a 4th (axes) input must be rejected")
	assert.Equal(t, ops.ErrInvalidOptionalInputCount(4, ops.NewBaseOperator(13, 2, 3, padTypeConstraints, "pad")), err)
}

func TestPadInputValidation(t *testing.T) {
	data := ops.TensorWithBackingFixture([]float32{1, 2, 3, 4}, 2, 2)

	tests := []struct {
		name   string
		inputs []tensor.Tensor
		errIs  bool
	}{
		{"data only", []tensor.Tensor{data}, true},
		{"data + pads", []tensor.Tensor{data, padsTensor(0, 0, 0, 0)}, false},
		{"data + pads + value", []tensor.Tensor{data, padsTensor(0, 0, 0, 0), ops.TensorWithBackingFixture([]float32{7}, 1)}, false},
		{"no inputs", []tensor.Tensor{}, true},
	}

	for _, test := range tests {
		p := padVersions[13]()
		_, err := p.ValidateInputs(test.inputs)

		if test.errIs {
			require.Error(t, err, test.name)
		} else {
			require.NoError(t, err, test.name)
		}
	}
}

// Ground truth: ONNX Pad is numpy.pad. Expected values below were computed from
// the vendored conformance data (test_data/test_{constant,edge,reflect}_pad) and
// cross-checked against numpy.pad; see .pi findings for SOP-74.
func TestPadConstant(t *testing.T) {
	tests := []struct {
		name     string
		data     tensor.Tensor
		pads     tensor.Tensor
		value    tensor.Tensor
		expShape []int
		expected []float32
	}{
		{
			// 1-D: [1,2,3] pad (2,1) value 0 => [0,0,1,2,3,0]
			name:     "1d default value",
			data:     ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
			pads:     padsTensor(2, 1),
			value:    nil,
			expShape: []int{6},
			expected: []float32{0, 0, 1, 2, 3, 0},
		},
		{
			// 1-D with an explicit rank-0 constant_value.
			name:     "1d explicit value",
			data:     ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
			pads:     padsTensor(1, 2),
			value:    tensor.New(tensor.FromScalar(float32(1.5))),
			expShape: []int{6},
			expected: []float32{1.5, 1, 2, 3, 1.5, 1.5},
		},
		{
			// 2-D: [[1,2],[3,4]] pads begins (1,0) ends (0,2)
			// => rows 3, cols 4:
			//   [0,0,0,0]
			//   [1,2,0,0]
			//   [3,4,0,0]
			name:     "2d asymmetric",
			data:     ops.TensorWithBackingFixture([]float32{1, 2, 3, 4}, 2, 2),
			pads:     padsTensor(1, 0, 0, 2),
			value:    nil,
			expShape: []int{3, 4},
			expected: []float32{0, 0, 0, 0, 1, 2, 0, 0, 3, 4, 0, 0},
		},
	}

	for _, test := range tests {
		p := padVersions[13]()
		require.NoError(t, p.Init(modeNode("constant")))

		inputs := []tensor.Tensor{test.data, test.pads}
		if test.value != nil {
			inputs = append(inputs, test.value)
		}

		inputs, err := p.ValidateInputs(inputs)
		require.NoError(t, err, test.name)

		res, err := p.Apply(inputs)
		require.NoError(t, err, test.name)
		require.Len(t, res, 1, test.name)

		assert.Equal(t, test.expShape, []int(res[0].Shape()), test.name)
		assert.Equal(t, test.expected, res[0].Data(), test.name)
	}
}

func TestPadEdge(t *testing.T) {
	// [1,2,3] pad (2,1) edge => [1,1,1,2,3,3]
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("edge")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
		padsTensor(2, 1),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []int{6}, []int(res[0].Shape()))
	assert.Equal(t, []float32{1, 1, 1, 2, 3, 3}, res[0].Data())
}

func TestPadEdge2D(t *testing.T) {
	// [[1,2],[3,4]] pads begins (1,1) ends (1,1), edge:
	//   [1,1,2,2]
	//   [1,1,2,2]
	//   [3,3,4,4]
	//   [3,3,4,4]
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("edge")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4}, 2, 2),
		padsTensor(1, 1, 1, 1),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []int{4, 4}, []int(res[0].Shape()))
	assert.Equal(t, []float32{
		1, 1, 2, 2,
		1, 1, 2, 2,
		3, 3, 4, 4,
		3, 3, 4, 4,
	}, res[0].Data())
}

// reflect must NOT repeat the edge element. Ground truth from test_reflect_pad:
// a row [0,0,0,-1,0] with pad (1,1) becomes [0, 0,0,0,-1,0, -1].
func TestPadReflect(t *testing.T) {
	// [1,2,3,4] pad (2,1) reflect => [3,2, 1,2,3,4, 3]
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("reflect")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4}, 4),
		padsTensor(2, 1),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []int{7}, []int(res[0].Shape()))
	assert.Equal(t, []float32{3, 2, 1, 2, 3, 4, 3}, res[0].Data())
}

// Exactly the ONNX ground-truth row from test_reflect_pad.
func TestPadReflectMatchesONNXGroundTruth(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("reflect")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]int32{0, 0, 0, -1, 0}, 5),
		padsTensor(1, 1),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []int{7}, []int(res[0].Shape()))
	assert.Equal(t, []int32{0, 0, 0, 0, -1, 0, -1}, res[0].Data())
}

// A reflect pad that reaches past the mirror is invalid in ONNX; it must error,
// not panic or read out of bounds.
func TestPadReflectPadTooLarge(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("reflect")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
		padsTensor(3, 0), // needs x[3] mirrored; only 3 elements exist
	})
	require.NoError(t, err)

	_, err = p.Apply(inputs)
	require.Error(t, err, "reflect pad >= dim size must be rejected")
}

// Negative pads crop the corresponding edge instead of padding it.
func TestPadNegativePadsCrop(t *testing.T) {
	// [1,2,3,4,5] pads (-1,-2) => [2,3]
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("constant")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4, 5}, 5),
		padsTensor(-1, -2),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []int{2}, []int(res[0].Shape()))
	assert.Equal(t, []float32{2, 3}, res[0].Data())
}

func TestPadMixedPadAndCrop(t *testing.T) {
	// [1,2,3,4] pads (2,-1) => pad 2 in front, drop 1 from the back
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("constant")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4}, 4),
		padsTensor(2, -1),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []int{5}, []int(res[0].Shape()))
	assert.Equal(t, []float32{0, 0, 1, 2, 3}, res[0].Data())
}

// pads must have exactly 2*rank entries.
func TestPadWrongPadsLength(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("constant")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4}, 2, 2),
		padsTensor(1, 1, 1), // rank 2 needs 4 entries
	})
	require.NoError(t, err)

	_, err = p.Apply(inputs)
	require.Error(t, err, "pads of length != 2*rank must be rejected")
}

// A zero pad on every axis is the identity.
func TestPadZeroIsIdentity(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("constant")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4}, 2, 2),
		padsTensor(0, 0, 0, 0),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []int{2, 2}, []int(res[0].Shape()))
	assert.Equal(t, []float32{1, 2, 3, 4}, res[0].Data())
}

// The Silero shape: rank-2 reflect pad on the last axis only, pads supplied as a
// runtime tensor. This is the exact node the VAD model runs.
func TestPadSileroReflectShape(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("reflect")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4, 5, 6, 7, 8}, 1, 8),
		padsTensor(0, 2, 0, 2),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []int{1, 12}, []int(res[0].Shape()))
	assert.Equal(t, []float32{3, 2, 1, 2, 3, 4, 5, 6, 7, 8, 7, 6}, res[0].Data())
}

// Reflect must mirror along EVERY axis, not just the innermost one. Every other
// reflect test here pads only the last axis (Silero's node pads (0,2,0,2)), so a
// last-axis-only implementation would slip through. onnxruntime ground truth:
// [[1,2],[3,4]] with pads (1,0,1,0) => [[3,4],[1,2],[3,4],[1,2]].
func TestPadReflectFirstAxis(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("reflect")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4}, 2, 2),
		padsTensor(1, 0, 1, 0),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []int{4, 2}, []int(res[0].Shape()))
	assert.Equal(t, []float32{3, 4, 1, 2, 3, 4, 1, 2}, res[0].Data())
}

// Cropping is mode-independent: a negative pad slices, whatever the mode. Verified
// against onnxruntime -- constant, edge and reflect all give [2,3] here. Only the
// constant path is covered above, so an implementation that handled crop solely
// inside the constant branch would pass the rest of the suite.
func TestPadNegativeCropIsModeIndependent(t *testing.T) {
	for _, mode := range []string{"constant", "edge", "reflect"} {
		p := padVersions[13]()
		require.NoError(t, p.Init(modeNode(mode)), mode)

		inputs, err := p.ValidateInputs([]tensor.Tensor{
			ops.TensorWithBackingFixture([]float32{1, 2, 3, 4, 5}, 5),
			padsTensor(-1, -2),
		})
		require.NoError(t, err, mode)

		res, err := p.Apply(inputs)
		require.NoError(t, err, mode)
		require.Len(t, res, 1, mode)

		assert.Equal(t, []int{2}, []int(res[0].Shape()), mode)
		assert.Equal(t, []float32{2, 3}, res[0].Data(), mode)
	}
}

// The largest legal reflect pad is dim-1. TestPadReflectPadTooLarge only pins the
// invalid side (pad == dim), so a too-strict guard (pad >= dim-1) would pass it while
// wrongly rejecting this. onnxruntime: [1,2,3] pads (2,0) => [3,2,1,2,3].
func TestPadReflectMaxValidPad(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("reflect")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
		padsTensor(2, 0),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err, "pad == dim-1 is legal for reflect")
	require.Len(t, res, 1)

	assert.Equal(t, []int{5}, []int(res[0].Shape()))
	assert.Equal(t, []float32{3, 2, 1, 2, 3}, res[0].Data())
}

// An explicitly empty mode string is a different code path from an absent attribute.
// ONNX defaults mode to "constant"; both spellings must reach it.
func TestPadEmptyModeStringIsConstant(t *testing.T) {
	p := padVersions[13]()

	node := &onnx.NodeProto{Attribute: []*onnx.AttributeProto{{Name: "mode", S: []byte("")}}}
	require.NoError(t, p.Init(node), "an empty mode string must mean constant")

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
		padsTensor(1, 0),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []float32{0, 1, 2, 3}, res[0].Data())
}

// constant_value is meaningless outside constant mode and must be ignored, not
// leaked into the reflected region. onnxruntime: [1,2,3] pads (1,1) reflect with
// value=9 => [2,1,2,3,2].
func TestPadConstantValueIgnoredInReflect(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("reflect")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
		padsTensor(1, 1),
		tensor.New(tensor.FromScalar(float32(9))),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, []float32{2, 1, 2, 3, 2}, res[0].Data())
}

// The reflect bound is against the axis size that SURVIVES the opposite side's crop,
// not the original size. Cropping happens first, so a begin-pad must be smaller than
// what is left after the end-crop, and vice versa. Getting this wrong mirrors into an
// element that was cropped away and returns a plausible WRONG tensor rather than an
// error. onnxruntime, on [1,2,3]:
//
//	pads (2,-1)  -> "pre-pad (2) exceeds maximum allowed (1)"
//	pads (-1,2)  -> "post-pad (2) exceeds maximum allowed (1)"
//	pads (-2,2)  -> "requires axis length >= 2 after slicing"
//	pads (1,-1)  -> [2,1,2]   (legal: begin pad 1 < surviving 2)
//	pads (-1,1)  -> [2,3,2]   (legal: end pad 1 < surviving 2)
func TestPadReflectBoundIsAgainstCroppedSize(t *testing.T) {
	rejected := [][]int64{{2, -1}, {-1, 2}, {-2, 2}}
	for _, pads := range rejected {
		p := padVersions[13]()
		require.NoError(t, p.Init(modeNode("reflect")))

		inputs, err := p.ValidateInputs([]tensor.Tensor{
			ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
			padsTensor(pads...),
		})
		require.NoError(t, err)

		_, err = p.Apply(inputs)
		require.Error(t, err, "reflect pads %v must be rejected: the crop shrinks the axis first", pads)
	}

	accepted := []struct {
		pads     []int64
		expected []float32
	}{
		{[]int64{1, -1}, []float32{2, 1, 2}},
		{[]int64{-1, 1}, []float32{2, 3, 2}},
	}

	for _, test := range accepted {
		p := padVersions[13]()
		require.NoError(t, p.Init(modeNode("reflect")))

		inputs, err := p.ValidateInputs([]tensor.Tensor{
			ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
			padsTensor(test.pads...),
		})
		require.NoError(t, err)

		res, err := p.Apply(inputs)
		require.NoError(t, err, "reflect pads %v is legal", test.pads)
		require.Len(t, res, 1)
		assert.Equal(t, test.expected, res[0].Data(), "reflect pads %v", test.pads)
	}
}

// edge replicates a surviving element, so a crop that removes the whole axis leaves it
// nothing to replicate. onnxruntime rejects; naively clamping would read a cropped-away
// element and return a plausible wrong tensor ([1..5] pads (-5,1) -> [5]).
func TestPadEdgeCropsEntireAxisRejected(t *testing.T) {
	for _, pads := range [][]int64{{-5, 1}, {1, -5}, {-5, 3}} {
		p := padVersions[13]()
		require.NoError(t, p.Init(modeNode("edge")))

		inputs, err := p.ValidateInputs([]tensor.Tensor{
			ops.TensorWithBackingFixture([]float32{1, 2, 3, 4, 5}, 5),
			padsTensor(pads...),
		})
		require.NoError(t, err)

		_, err = p.Apply(inputs)
		require.Error(t, err, "edge pads %v crop the axis away entirely", pads)
	}

	// A crop that leaves one element is fine: edge replicates it.
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("edge")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3, 4, 5}, 5),
		padsTensor(-4, 2),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, []float32{5, 5, 5}, res[0].Data())
}

// constant_value of length 0 must error, not panic — the same gorgonia Data() trap
// readPads already guards against.
func TestPadEmptyConstantValueRejected(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("constant")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
		padsTensor(1, 0),
		ops.TensorWithBackingFixture([]float32{}, 0),
	})
	require.NoError(t, err)

	require.NotPanics(t, func() {
		_, err = p.Apply(inputs)
	})
	require.Error(t, err, "an empty constant_value must be rejected")
}

// An empty pads tensor must error, not panic. gorgonia panics if Data() is called
// on a zero-length tensor, so the length has to be checked on the shape first.
func TestPadEmptyPadsRejected(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("constant")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]float32{1, 2, 3}, 3),
		ops.TensorWithBackingFixture([]int64{}, 0),
	})
	require.NoError(t, err)

	require.NotPanics(t, func() {
		_, err = p.Apply(inputs)
	})
	require.Error(t, err, "an empty pads tensor must be rejected")
}

// Rank-0 data has no axes to pad. Reject it rather than indexing an empty shape.
func TestPadRank0DataRejected(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("constant")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		tensor.New(tensor.FromScalar(float32(5))),
		ops.TensorWithBackingFixture([]int64{}, 0),
	})
	require.NoError(t, err)

	require.NotPanics(t, func() {
		_, err = p.Apply(inputs)
	})
	require.Error(t, err, "rank-0 data must be rejected")
}

// Pad accepts non-float data; the output keeps the input's dtype.
func TestPadPreservesDtype(t *testing.T) {
	p := padVersions[13]()
	require.NoError(t, p.Init(modeNode("constant")))

	inputs, err := p.ValidateInputs([]tensor.Tensor{
		ops.TensorWithBackingFixture([]int32{1, 2}, 2),
		padsTensor(1, 0),
	})
	require.NoError(t, err)

	res, err := p.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, tensor.Int32, res[0].Dtype())
	assert.Equal(t, []int32{0, 1, 2}, res[0].Data())
}
