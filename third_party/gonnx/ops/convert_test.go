package ops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gorgonia.org/tensor"
)

// TestConvertScalarUintDtype is the SOP-91 regression: a rank-0 scalar uint
// tensor used to panic in ConvertTensorDtype ("interface {} is uint8, not
// []uint8") because IfScalarToSlice had no uint cases and left the bare scalar
// unwrapped. Sibling of the scalar-bool bug fixed in SOP-86.
func TestConvertScalarUintDtype(t *testing.T) {
	cases := []struct {
		name string
		in   tensor.Tensor
	}{
		{"uint8", tensor.New(tensor.FromScalar(uint8(5)))},
		{"uint16", tensor.New(tensor.FromScalar(uint16(5)))},
		{"uint32", tensor.New(tensor.FromScalar(uint32(5)))},
		{"uint64", tensor.New(tensor.FromScalar(uint64(5)))},
	}

	for _, c := range cases {
		var (
			out tensor.Tensor
			err error
		)

		assert.NotPanicsf(t, func() { out, err = ConvertTensorDtype(c.in, 1) }, "scalar %s Cast panicked", c.name)
		assert.NoErrorf(t, err, "scalar %s Cast", c.name)

		if out != nil {
			assert.Equalf(t, float32(5), IfScalarToSlice(out.Data()).([]float32)[0], "scalar %s value round-trip", c.name)
		}
	}
}

func TestConvertTensorDtype(t *testing.T) {
	tests := []struct {
		tensorIn  tensor.Tensor
		tensorOut tensor.Tensor
		newType   int32
		err       error
	}{
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]float32{1.0, 2.0})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]float64{1.0, 2.0})),
			11,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]float64{1.0, 2.0})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]float32{1.0, 2.0})),
			1,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]int8{1, 2})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]uint16{1, 2})),
			4,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]int16{1, 2})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]uint8{1, 2})),
			2,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]int32{1, 2})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]uint64{1, 2})),
			13,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]int64{1, 2})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]int8{1, 2})),
			3,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]uint8{1, 2})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]int16{1, 2})),
			5,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]uint16{1, 2})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]int32{1, 2})),
			6,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]uint32{1, 2})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]float32{1.0, 2.0})),
			1,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]uint64{1, 2})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]uint32{1, 2})),
			12,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]float32{1.0, 2.0})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]int64{1, 2})),
			7,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]int64{1, 2})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]float32{1.0, 2.0})),
			1,
			nil,
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]string{"joe", "joe"})),
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]float32{1.0, 2.0})),
			1,
			ErrConversionInvalidType(tensor.String, 1),
		},
		{
			tensor.New(tensor.WithShape(2), tensor.WithBacking([]float32{1.0, 2.1})),
			nil,
			8,
			ErrConversionNotSupported(8),
		},
	}

	for _, test := range tests {
		out, err := ConvertTensorDtype(test.tensorIn, test.newType)

		assert.Equal(t, test.err, err)

		if test.err != nil {
			continue
		}

		assert.Equal(t, test.tensorOut, out)
	}
}

func TestCreateNewBacking(t *testing.T) {
	assert.InDeltaSlice(t, []float64{0.5, 0.8}, createNewBacking[float32, float64]([]float32{0.5, 0.8}), 0.00001)
	assert.Equal(t, []int32{1, 2}, createNewBacking[float32, int32]([]float32{1.2, 2.5}))
	assert.Equal(t, []float32{1.0, 2.0}, createNewBacking[int64, float32]([]int64{1, 2}))
}
