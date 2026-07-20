package size

import (
	"testing"

	"github.com/advancedclimatesystems/gonnx/ops"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorgonia.org/tensor"
)

func TestSizeInit(t *testing.T) {
	s := &Size{}

	// 'size' has no attributes, so passing nil must not fail initialization.
	err := s.Init(nil)
	assert.Nil(t, err)
}

func TestSize(t *testing.T) {
	tests := []struct {
		version    int64
		inputShape []int
		expected   int64
	}{
		{1, []int{1, 2, 3, 4}, 24},
		{13, []int{2, 3}, 6},
		{13, []int{5}, 5},
		{13, []int{1}, 1},
	}

	for _, test := range tests {
		size := sizeVersions[test.version]()
		inputs := []tensor.Tensor{
			ops.Float32TensorFixture(test.inputShape...),
		}

		res, err := size.Apply(inputs)
		require.NoError(t, err)
		require.Len(t, res, 1)

		assert.Equal(t, test.expected, res[0].Data())
	}
}

// Size accepts every tensor type, and always returns an int64 count.
func TestSizeReturnsInt64ForNonFloatInput(t *testing.T) {
	size := sizeVersions[13]()
	inputs := []tensor.Tensor{ops.TensorWithBackingFixture([]int32{1, 2, 3, 4, 5, 6}, 6)}

	res, err := size.Apply(inputs)
	require.NoError(t, err)
	require.Len(t, res, 1)

	assert.Equal(t, int64(6), res[0].Data())
}

func TestInputValidationSize(t *testing.T) {
	tests := []struct {
		version int64
		inputs  []tensor.Tensor
		err     error
	}{
		{
			1,
			[]tensor.Tensor{ops.TensorWithBackingFixture([]uint32{3, 4}, 2)},
			nil,
		},
		{
			13,
			[]tensor.Tensor{ops.TensorWithBackingFixture([]float32{3, 4}, 2)},
			nil,
		},
		{
			13,
			[]tensor.Tensor{},
			ops.ErrInvalidInputCount(0, size13BaseOpFixture()),
		},
	}

	for _, test := range tests {
		size := sizeVersions[test.version]()
		validated, err := size.ValidateInputs(test.inputs)

		assert.Equal(t, test.err, err)

		if test.err == nil {
			assert.Equal(t, test.inputs, validated)
		}
	}
}

func size13BaseOpFixture() ops.BaseOperator {
	return ops.NewBaseOperator(13, 1, 1, sizeTypeConstraints, "size")
}
