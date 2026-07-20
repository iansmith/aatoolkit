package reshape

import (
	"testing"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"github.com/stretchr/testify/assert"
	"gorgonia.org/tensor"
)

func TestReshapeInit(t *testing.T) {
	tests := []struct {
		version int64
		err     error
	}{
		{5, nil},
		{13, nil},
	}

	for _, test := range tests {
		r := reshapeVersions[test.version]()
		err := r.Init(nil)
		assert.Equal(t, test.err, err)
	}
}

// allowzero is the opset-14 addition to Reshape. allowzero=0 is the opset-13
// default and must still be accepted; any non-zero value changes how zero dims
// are handled and is unimplemented, so it must fail loudly. Rejecting only the
// literal 1 would silently accept allowzero=2, so both are pinned.
func TestReshapeInitRejectsAllowzero(t *testing.T) {
	r := &Reshape{}

	for _, v := range []int64{1, 2} {
		err := r.Init(&onnx.NodeProto{Attribute: []*onnx.AttributeProto{{Name: "allowzero", I: v}}})
		assert.Equal(t, ops.ErrUnsupportedAttribute("allowzero", r), err)
	}

	err := r.Init(&onnx.NodeProto{Attribute: []*onnx.AttributeProto{{Name: "allowzero", I: 0}}})
	assert.Nil(t, err)
}

// A genuinely unknown post-13 attribute must fail loudly rather than fall through
// to the pre-guard "return nil" — otherwise the guard only catches allowzero and
// silently ignores everything else.
func TestReshapeInitRejectsUnknownAttr(t *testing.T) {
	r := &Reshape{}
	err := r.Init(&onnx.NodeProto{Attribute: []*onnx.AttributeProto{{Name: "unknown"}}})
	assert.Equal(t, ops.ErrInvalidAttribute("unknown", r), err)
}

func TestReshape(t *testing.T) {
	tests := []struct {
		version    int64
		inputShape []int
		newShape   []int64
		expected   tensor.Shape
	}{
		{
			5,
			[]int{2, 3},
			[]int64{1, 6},
			[]int{1, 6},
		},
		{
			13,
			[]int{1, 2, 3},
			[]int64{0, 2, 3},
			[]int{1, 2, 3},
		},
		{
			13,
			[]int{1, 2, 3},
			[]int64{1, -1, 2},
			[]int{1, 3, 2},
		},
		{
			13,
			[]int{1, 2, 3},
			[]int64{1, -1},
			[]int{1, 6},
		},
		{
			13,
			[]int{3, 4, 2},
			[]int64{1, 0, -1},
			[]int{1, 4, 6},
		},
	}

	for _, test := range tests {
		reshape := reshapeVersions[test.version]()
		inputs := []tensor.Tensor{
			ops.Float32TensorFixture(test.inputShape...),
			tensor.New(tensor.WithBacking(test.newShape)),
		}
		res, err := reshape.Apply(inputs)
		assert.Nil(t, err)
		assert.Equal(t, test.expected, res[0].Shape())
	}
}

func TestInputValidationReshape(t *testing.T) {
	tests := []struct {
		version int64
		inputs  []tensor.Tensor
		err     error
	}{
		{
			5,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]uint32{1, 2}, 2),
				ops.TensorWithBackingFixture([]int64{3, 4}, 2),
			},
			nil,
		},
		{
			13,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float64{1, 2}, 2),
				ops.TensorWithBackingFixture([]int64{3, 4}, 2),
			},
			nil,
		},
		{
			5,
			[]tensor.Tensor{ops.TensorWithBackingFixture([]int{1, 2}, 2)},
			ops.ErrInvalidInputCount(1, reshape5BaseOpFixture()),
		},
		{
			13,
			[]tensor.Tensor{ops.TensorWithBackingFixture([]int{1, 2}, 2)},
			ops.ErrInvalidInputCount(1, reshape13BaseOpFixture()),
		},
		{
			13,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float64{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{3, 4}, 2),
			},
			ops.ErrInvalidInputType(1, "int", reshape13BaseOpFixture()),
		},
	}

	for _, test := range tests {
		reshape := reshapeVersions[test.version]()
		validated, err := reshape.ValidateInputs(test.inputs)

		assert.Equal(t, test.err, err)

		if test.err == nil {
			assert.Equal(t, test.inputs, validated)
		}
	}
}

func reshape5BaseOpFixture() ops.BaseOperator {
	return ops.NewBaseOperator(5, 2, 2, reshapeTypeConstraints, "reshape")
}

func reshape13BaseOpFixture() ops.BaseOperator {
	return ops.NewBaseOperator(13, 2, 2, reshapeTypeConstraints, "reshape")
}
