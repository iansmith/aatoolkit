package conv

import (
	"testing"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"github.com/stretchr/testify/assert"
	"gorgonia.org/tensor"
)

func TestConvInit(t *testing.T) {
	c := &Conv{}
	err := c.Init(Conv2DOnnxNodeProtoFixture())

	assert.Nil(t, err)

	var expectedAutopad AutoPadSetting = "VALID"

	assert.Equal(t, expectedAutopad, c.autoPad)
	assert.Equal(t, []int{1, 1}, c.dilations)
	assert.Equal(t, []int{2, 2}, c.kernelShape)
	assert.Equal(t, []int{1, 2}, c.pads)
	assert.Equal(t, []int{1, 1}, c.strides)
}

func TestConvInitUnsupported(t *testing.T) {
	c := &Conv{}
	err := c.Init(ConvUnsupportedOnnxNodeProtoFixture())

	assert.Equal(
		t,
		err,
		ops.ErrUnsupportedAttribute("group", c),
	)
}

func TestConv(t *testing.T) {
	tests := []struct {
		version       int64
		node          *onnx.NodeProto
		shapes        [][]int
		backings      [][]float32
		expectedShape tensor.Shape
		expected      []float32
	}{
		// Test 1D Convolution.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{3}},
					{Name: "pads", Ints: []int64{0, 0}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 1, 6}, {1, 1, 3}},
			[][]float32{{0, 1, 2, 3, 4, 5}, {1, 1, 1}},
			[]int{1, 1, 4},
			[]float32{3, 6, 9, 12},
		},
		// Test 2D Convolution.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{0, 0, 0, 0}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 1, 3, 3}, {1, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8}, {1, 1, 1, 1}},
			[]int{1, 1, 2, 2},
			[]float32{8, 12, 20, 24},
		},
		// Test SAME_LOWER autopad setting.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("SAME_LOWER")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 1, 3, 3}, {1, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8}, {1, 1, 1, 1}},
			[]int{1, 1, 3, 3},
			[]float32{0, 1, 3, 3, 8, 12, 9, 20, 24},
		},
		// Test SAME_UPPER autopad setting.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("SAME_UPPER")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 1, 3, 3}, {1, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8}, {1, 1, 1, 1}},
			[]int{1, 1, 3, 3},
			[]float32{8, 12, 7, 20, 24, 13, 13, 15, 8},
		},
		// Test VALID autopad setting.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("VALID")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 1, 3, 3}, {1, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8}, {1, 1, 1, 1}},
			[]int{1, 1, 3, 3},
			[]float32{8, 12, 7, 20, 24, 13, 13, 15, 8},
		},
		// Test dilation attribute.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{2, 2}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{0, 0, 0, 0}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 1, 4, 4}, {1, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, {1, 1, 1, 1}},
			[]int{1, 1, 2, 2},
			[]float32{20, 24, 36, 40},
		},
		// Test pads attribute.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{1, 1}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{1, 1, 2, 2}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 1, 2, 2}, {1, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3}, {1, 1, 1, 1}},
			[]int{1, 1, 4, 4},
			[]float32{0, 1, 1, 0, 2, 6, 4, 0, 2, 5, 3, 0, 0, 0, 0, 0},
		},
		// Test strides attribute.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{0, 0, 0, 0}},
					{Name: "strides", Ints: []int64{2, 2}},
				},
			},
			[][]int{{1, 1, 4, 4}, {1, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, {1, 1, 1, 1}},
			[]int{1, 1, 2, 2},
			[]float32{10, 18, 42, 50},
		},
		// Test batch dimension.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{0, 0, 0, 0}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{2, 1, 3, 3}, {1, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17}, {1, 1, 1, 1}},
			[]int{2, 1, 2, 2},
			[]float32{8, 12, 20, 24, 44, 48, 56, 60},
		},
		// Test 2D convolution with multiple channels.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{0, 0, 0, 0}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 2, 3, 3}, {1, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17}, {1, 1, 1, 1}},
			[]int{1, 1, 2, 2},
			[]float32{52, 60, 76, 84},
		},
		// Test multiple kernels.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{0, 0, 0, 0}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 1, 3, 3}, {2, 1, 2, 2}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8}, {1, 1, 1, 1, 2, 2, 2, 2}},
			[]int{1, 2, 2, 2},
			[]float32{8, 12, 20, 24, 16, 24, 40, 48},
		},
		// Test bias.
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{2, 2}},
					{Name: "pads", Ints: []int64{0, 0, 0, 0}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 1, 3, 3}, {1, 1, 2, 2}, {1}},
			[][]float32{{0, 1, 2, 3, 4, 5, 6, 7, 8}, {1, 1, 1, 1}, {0.5}},
			[]int{1, 1, 2, 2},
			[]float32{8.5, 12.5, 20.5, 24.5},
		},
		// Test a 2D convolution whose input is wider than it is tall (W > H).
		// Regression for SOP-90: applyConv2D's inner width loop bounded by the
		// padded H instead of the padded W, so the rightmost output column(s)
		// were left at their zero-init value. Here H=1, W=3, kernel 1x2 over two
		// channels, stride 1 -> output width 2. Column 1 (47) must be computed;
		// the bug returned [37, 0].
		{
			11,
			&onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "auto_pad", S: []byte("NOTSET")},
					{Name: "dilations", Ints: []int64{}},
					{Name: "group", I: 1},
					{Name: "kernel_shape", Ints: []int64{1, 2}},
					{Name: "pads", Ints: []int64{0, 0, 0, 0}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			[][]int{{1, 2, 1, 3}, {1, 2, 1, 2}},
			[][]float32{{1, 2, 3, 4, 5, 6}, {1, 2, 3, 4}},
			[]int{1, 1, 1, 2},
			[]float32{37, 47},
		},
	}

	for _, test := range tests {
		inputs := []tensor.Tensor{
			ops.TensorWithBackingFixture(test.backings[0], test.shapes[0]...),
			ops.TensorWithBackingFixture(test.backings[1], test.shapes[1]...),
			nil,
		}

		if len(test.backings) == 3 {
			inputs[2] = ops.TensorWithBackingFixture(test.backings[2], test.shapes[2]...)
		}

		conv := convVersions[test.version]()
		err := conv.Init(test.node)
		assert.Nil(t, err)

		res, err := conv.Apply(inputs)
		assert.Nil(t, err)

		assert.Equal(t, test.expectedShape, res[0].Shape())
		assert.Equal(t, test.expected, res[0].Data())
	}
}

func TestInputValidationConv(t *testing.T) {
	tests := []struct {
		version int64
		inputs  []tensor.Tensor
		err     error
	}{
		{
			11,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float32{1, 2}, 2),
				ops.TensorWithBackingFixture([]float32{3, 4}, 2),
				nil,
			},
			nil,
		},
		{
			11,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]float64{1, 2}, 2),
				ops.TensorWithBackingFixture([]float64{3, 4}, 2),
				ops.TensorWithBackingFixture([]float64{5, 6}, 2),
			},
			nil,
		},
		{
			11,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
			},
			ops.ErrInvalidOptionalInputCount(1, conv11BaseOpFixture()),
		},
		{
			11,
			[]tensor.Tensor{
				ops.TensorWithBackingFixture([]int{1, 2}, 2),
				ops.TensorWithBackingFixture([]int{3, 4}, 2),
			},
			ops.ErrInvalidInputType(0, "int", conv11BaseOpFixture()),
		},
	}

	for _, test := range tests {
		conv := convVersions[test.version]()
		validated, err := conv.ValidateInputs(test.inputs)

		assert.Equal(t, test.err, err)

		if test.err == nil {
			assert.Equal(t, test.inputs, validated)
		}
	}
}

func TestSetDefaultDilations(t *testing.T) {
	c := &Conv{}
	x := ops.TensorWithBackingFixture([]float32{0, 1, 2, 3, 4, 5, 6, 7, 8}, 1, 1, 3, 3)

	c.setDefaultDilations(x)

	assert.Equal(t, []int{1, 1}, c.dilations)
}

func TestSetKernelShape(t *testing.T) {
	c := &Conv{}
	kernel := ops.TensorWithBackingFixture([]float32{0, 1, 2, 3}, 1, 1, 2, 2)

	c.setKernelShape(kernel)

	assert.Equal(t, []int{2, 2}, c.kernelShape)
}

func TestSetDefaultPaddings(t *testing.T) {
	c := &Conv{}
	x := ops.TensorWithBackingFixture([]float32{0, 1, 2, 3, 4, 5, 6, 7, 8}, 1, 1, 3, 3)

	c.setDefaultPaddings(x)

	assert.Equal(t, []int{0, 0, 0, 0}, c.pads)
}

func TestSetDefaultStrides(t *testing.T) {
	c := &Conv{}
	x := ops.TensorWithBackingFixture([]float32{0, 1, 2, 3, 4, 5, 6, 7, 8}, 1, 1, 3, 3)

	c.setDefaultStrides(x)

	assert.Equal(t, []int{1, 1}, c.strides)
}

func TestSetPaddingWithAutoPad(t *testing.T) {
	x := ops.TensorWithBackingFixture([]float32{0, 1, 2, 3, 4, 5, 6, 7, 8}, 1, 1, 3, 3)

	tests := []struct {
		setting      AutoPadSetting
		expectedPads []int
	}{
		{"NOTSET", []int{0, 0, 0, 0}},
		{"SAME_LOWER", []int{1, 1, 0, 0}},
		{"SAME_UPPER", []int{0, 0, 1, 1}},
		{"VALID", []int{0, 0, 1, 1}},
	}

	for _, test := range tests {
		conv := &Conv{
			autoPad:     test.setting,
			pads:        []int{0, 0, 0, 0},
			kernelShape: []int{2, 2},
			strides:     []int{1, 1},
		}
		conv.setPaddingWithAutoPad(x)

		assert.Equal(t, test.expectedPads, conv.pads)
	}
}

func TestGetDilatedKernel(t *testing.T) {
	tests := []struct {
		dilations       []int
		kernelShape     []int
		kernelBacking   []float32
		expectedShape   tensor.Shape
		expectedBacking []float32
	}{
		{
			[]int{1},
			[]int{1, 1, 3},
			[]float32{1, 1, 1},
			[]int{1, 1, 3},
			[]float32{1, 1, 1},
		},
		{
			[]int{2},
			[]int{1, 1, 3},
			[]float32{1, 1, 1},
			[]int{1, 1, 5},
			[]float32{1, 0, 1, 0, 1},
		},
		{
			[]int{2, 1},
			[]int{1, 1, 2, 2},
			[]float32{1, 1, 1, 1},
			[]int{1, 1, 3, 2},
			[]float32{1, 1, 0, 0, 1, 1},
		},
		{
			[]int{1, 2},
			[]int{1, 1, 2, 2},
			[]float32{1, 1, 1, 1},
			[]int{1, 1, 2, 3},
			[]float32{1, 0, 1, 1, 0, 1},
		},
		{
			[]int{2, 2},
			[]int{1, 1, 3, 3},
			[]float32{0, 1, 2, 3, 4, 5, 6, 7, 8},
			[]int{1, 1, 5, 5},
			[]float32{0, 0, 1, 0, 2, 0, 0, 0, 0, 0, 3, 0, 4, 0, 5, 0, 0, 0, 0, 0, 6, 0, 7, 0, 8},
		},
		{
			[]int{3, 2},
			[]int{1, 1, 2, 3},
			[]float32{1, 2, 3, 4, 5, 6},
			[]int{1, 1, 4, 5},
			[]float32{1, 0, 2, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 0, 5, 0, 6},
		},
	}

	for _, test := range tests {
		conv := &Conv{
			dilations:   test.dilations,
			kernelShape: []int{2, 2},
		}
		kernel := ops.TensorWithBackingFixture(test.kernelBacking, test.kernelShape...)

		dilatedKernel, err := conv.getDilatedKernel(kernel)
		assert.Nil(t, err)

		assert.Equal(t, test.expectedShape, dilatedKernel.Shape())
		assert.Equal(t, test.expectedBacking, dilatedKernel.Data())
	}
}

func TestGetOutputShape(t *testing.T) {
	tests := []struct {
		conv          *Conv
		xShape        []int
		xBacking      []float32
		kernelShape   []int
		kernelBacking []float32
		expected      tensor.Shape
	}{
		{
			&Conv{
				kernelShape: []int{3},
				pads:        []int{0, 0},
				strides:     []int{1},
			},
			[]int{1, 1, 6},
			[]float32{0, 1, 2, 3, 4, 5},
			[]int{1, 1, 3},
			[]float32{1, 1, 1},
			[]int{1, 1, 4},
		},
		{
			&Conv{
				kernelShape: []int{3},
				pads:        []int{1, 2},
				strides:     []int{2},
			},
			[]int{1, 1, 6},
			[]float32{0, 1, 2, 3, 4, 5},
			[]int{1, 1, 3},
			[]float32{1, 1, 1},
			[]int{1, 1, 4},
		},
		{
			&Conv{
				kernelShape: []int{2, 2},
				pads:        []int{1, 2, 1, 2},
				strides:     []int{2, 1},
			},
			[]int{1, 1, 4, 4},
			[]float32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			[]int{1, 1, 2, 2},
			[]float32{1, 1, 1, 1},
			[]int{1, 1, 3, 7},
		},
		{
			&Conv{
				kernelShape: []int{2, 2},
				pads:        []int{0, 0, 0, 0},
				strides:     []int{1, 1},
			},
			[]int{1, 1, 4, 4},
			[]float32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			[]int{1, 1, 2, 2},
			[]float32{1, 1, 1, 1},
			[]int{1, 1, 3, 3},
		},
	}

	for _, test := range tests {
		outputShape := test.conv.getOutputShape(
			ops.TensorWithBackingFixture(test.xBacking, test.xShape...),
			ops.TensorWithBackingFixture(test.kernelBacking, test.kernelShape...),
		)

		assert.Equal(t, test.expected, outputShape)
	}
}

func TestPadInput(t *testing.T) {
	tests := []struct {
		conv            *Conv
		xShape          []int
		xBacking        []float32
		expectedShape   tensor.Shape
		expectedBacking []float32
	}{
		{
			&Conv{
				pads: []int{0, 0},
			},
			[]int{1, 1, 6},
			[]float32{0, 1, 2, 3, 4, 5},
			[]int{1, 1, 6},
			[]float32{0, 1, 2, 3, 4, 5},
		},
		{
			&Conv{
				pads: []int{1, 2},
			},
			[]int{1, 1, 6},
			[]float32{0, 1, 2, 3, 4, 5},
			[]int{1, 1, 9},
			[]float32{0, 0, 1, 2, 3, 4, 5, 0, 0},
		},
		{
			&Conv{
				pads: []int{1, 1, 1, 1},
			},
			[]int{1, 1, 2, 2},
			[]float32{1, 2, 3, 4},
			[]int{1, 1, 4, 4},
			[]float32{0, 0, 0, 0, 0, 1, 2, 0, 0, 3, 4, 0, 0, 0, 0, 0},
		},
		{
			&Conv{
				pads: []int{1, 0, 2, 0},
			},
			[]int{1, 1, 2, 2},
			[]float32{1, 2, 3, 4},
			[]int{1, 1, 5, 2},
			[]float32{0, 0, 1, 2, 3, 4, 0, 0, 0, 0},
		},
	}

	for _, test := range tests {
		paddedX, err := test.conv.padInput(
			ops.TensorWithBackingFixture(test.xBacking, test.xShape...),
		)

		assert.Nil(t, err)
		assert.Equal(t, test.expectedShape, paddedX.Shape())
		assert.Equal(t, test.expectedBacking, paddedX.Data())
	}
}

func TestGetSubImage(t *testing.T) {
	tests := []struct {
		conv               *Conv
		xShape             []int
		xBacking           []float32
		batchIdx           int
		startSpatialCoords []int
		expectedShape      tensor.Shape
		expectedBacking    []float32
	}{
		{
			&Conv{kernelShape: []int{2}},
			[]int{1, 1, 3},
			[]float32{0, 1, 2},
			0,
			[]int{0},
			[]int{1, 2},
			[]float32{0, 1},
		},
		{
			&Conv{kernelShape: []int{2}},
			[]int{1, 2, 3},
			[]float32{0, 1, 2, 3, 4, 5},
			0,
			[]int{0},
			[]int{2, 2},
			[]float32{0, 1, 3, 4},
		},
		{
			&Conv{kernelShape: []int{2, 2}},
			[]int{1, 1, 3, 3},
			[]float32{0, 1, 2, 3, 4, 5, 6, 7, 8},
			0,
			[]int{0, 0},
			[]int{1, 2, 2},
			[]float32{0, 1, 3, 4},
		},
		{
			&Conv{kernelShape: []int{2, 2}},
			[]int{1, 1, 3, 3},
			[]float32{0, 1, 2, 3, 4, 5, 6, 7, 8},
			0,
			[]int{1, 1},
			[]int{1, 2, 2},
			[]float32{4, 5, 7, 8},
		},
		{
			&Conv{kernelShape: []int{2}},
			[]int{2, 1, 3},
			[]float32{0, 1, 2, 3, 4, 5},
			1,
			[]int{1},
			[]int{1, 2},
			[]float32{4, 5},
		},
	}

	for _, test := range tests {
		subImage, err := test.conv.getSubImage(
			ops.TensorWithBackingFixture(test.xBacking, test.xShape...),
			test.batchIdx,
			test.startSpatialCoords...,
		)

		assert.Nil(t, err)
		assert.Equal(t, test.expectedShape, subImage.Shape())
		assert.Equal(t, test.expectedBacking, subImage.Data())
	}
}

func TestAddBias(t *testing.T) {
	tests := []struct {
		conv        *Conv
		outShape    []int
		outBacking  []float32
		biasShape   []int
		biasBacking []float32
		expected    []float32
	}{
		{
			&Conv{},
			[]int{1, 1, 3},
			[]float32{0, 1, 2},
			[]int{1},
			[]float32{0.5},
			[]float32{0.5, 1.5, 2.5},
		},
		{
			&Conv{},
			[]int{1, 1, 3, 3},
			[]float32{0, 1, 2, 3, 4, 5, 6, 7, 8},
			[]int{1},
			[]float32{0.5},
			[]float32{0.5, 1.5, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5},
		},
		{
			&Conv{},
			[]int{1, 2, 2, 2},
			[]float32{0, 1, 2, 3, 4, 5, 6, 7},
			[]int{2},
			[]float32{-1, 1},
			[]float32{-1, 0, 1, 2, 5, 6, 7, 8},
		},
		{
			&Conv{},
			[]int{2, 2, 2, 2},
			[]float32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			[]int{2},
			[]float32{-1, 1},
			[]float32{-1, 0, 1, 2, 5, 6, 7, 8, 7, 8, 9, 10, 13, 14, 15, 16},
		},
	}

	for _, test := range tests {
		out, err := test.conv.addBias(
			ops.TensorWithBackingFixture(test.outBacking, test.outShape...),
			ops.TensorWithBackingFixture(test.biasBacking, test.biasShape...),
		)

		assert.Nil(t, err)
		assert.Equal(t, test.expected, out.Data())
	}
}

func Conv2DOnnxNodeProtoFixture() *onnx.NodeProto {
	return &onnx.NodeProto{
		Attribute: []*onnx.AttributeProto{
			{Name: "auto_pad", S: []byte("VALID")},
			{Name: "dilations", Ints: []int64{1, 1}},
			{Name: "kernel_shape", Ints: []int64{2, 2}},
			{Name: "pads", Ints: []int64{1, 2}},
			{Name: "strides", Ints: []int64{1, 1}},
		},
	}
}

func ConvUnsupportedOnnxNodeProtoFixture() *onnx.NodeProto {
	return &onnx.NodeProto{
		Attribute: []*onnx.AttributeProto{
			{Name: "group", I: 2},
		},
	}
}

func conv11BaseOpFixture() ops.BaseOperator {
	return ops.NewBaseOperator(11, 2, 3, convTypeConstraints, "conv")
}

// TestConvUnitAxes exercises convolutions whose sub-image and sub-kernel both
// collapse to unit spatial/kernel axes — the 1x1 "pointwise" conv the Silero VAD
// decoder uses as its final classifier. gorgonia's Dense.Slice drops every sliced
// length-1 axis, so the sub-image loses its unit spatial axis while the sub-kernel
// keeps a unit kernel axis, leaving the internal elementwise multiply with
// rank-inconsistent operands (e.g. [128] vs [128,1]) that UnidirectionalBroadcast
// cannot reconcile. See SOP-88.
func TestConvUnitAxes(t *testing.T) {
	tests := []struct {
		name          string
		node          *onnx.NodeProto
		shapes        [][]int
		backings      [][]float32
		expectedShape tensor.Shape
		expected      []float32
	}{
		{
			// Silero decoder final conv shape: 1x1 Conv1D over 128 channels, one
			// spatial position. Verifies behaviors 1 & 5 (shape [1,1,1], no error).
			name: "1x1 Conv1D, Silero decoder shape [1,128,1]",
			node: &onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "kernel_shape", Ints: []int64{1}},
					{Name: "pads", Ints: []int64{0, 0}},
					{Name: "strides", Ints: []int64{1}},
				},
			},
			shapes:        [][]int{{1, 128, 1}, {1, 128, 1}, {1}},
			backings:      [][]float32{onesF32(128), onesF32(128), {0.25}},
			expectedShape: tensor.Shape{1, 1, 1},
			// sum over 128 channels of 1*1, plus bias 0.25 = 128.25
			expected: []float32{128.25},
		},
		{
			// Small 1x1 Conv1D with distinct weights so the channelwise sum is a
			// non-trivial hand-checkable reference (behavior 2).
			name: "1x1 Conv1D, distinct channel weights",
			node: &onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "kernel_shape", Ints: []int64{1}},
					{Name: "pads", Ints: []int64{0, 0}},
					{Name: "strides", Ints: []int64{1}},
				},
			},
			shapes:   [][]int{{1, 3, 1}, {1, 3, 1}, {1}},
			backings: [][]float32{{2, 3, 4}, {5, 6, 7}, {0.5}},
			// 2*5 + 3*6 + 4*7 + 0.5 = 56.5
			expectedShape: tensor.Shape{1, 1, 1},
			expected:      []float32{56.5},
		},
		{
			// Unit kernel (kernel_shape=1) but MULTIPLE spatial positions: the
			// collapse recurs at every output position, so the fix must live inside
			// the position loop, not just handle the single-position case. No bias —
			// the collapse happens before addBias runs.
			name: "1x1 Conv1D, kernel=1 over H=3, no bias",
			node: &onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "kernel_shape", Ints: []int64{1}},
					{Name: "pads", Ints: []int64{0, 0}},
					{Name: "strides", Ints: []int64{1}},
				},
			},
			shapes:   [][]int{{1, 2, 3}, {1, 2, 1}},
			backings: [][]float32{{1, 2, 3, 4, 5, 6}, {2, 3}},
			// ch0={1,2,3} w0=2, ch1={4,5,6} w1=3:
			// h0=1*2+4*3=14, h1=2*2+5*3=19, h2=3*2+6*3=24
			expectedShape: tensor.Shape{1, 1, 3},
			expected:      []float32{14, 19, 24},
		},
		{
			// Multiple kernels (M=2): reconciliation must hold per-kernel.
			name: "1x1 Conv1D, 2 kernels with bias",
			node: &onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "kernel_shape", Ints: []int64{1}},
					{Name: "pads", Ints: []int64{0, 0}},
					{Name: "strides", Ints: []int64{1}},
				},
			},
			shapes:   [][]int{{1, 2, 1}, {2, 2, 1}, {2}},
			backings: [][]float32{{3, 4}, {5, 6, 7, 8}, {1, 10}},
			// k0: 3*5+4*6+1=40, k1: 3*7+4*8+10=63
			expectedShape: tensor.Shape{1, 2, 1},
			expected:      []float32{40, 63},
		},
		{
			// 1x1 Conv2D whose sub-image collapses to [C] while the sub-kernel keeps
			// unit kernel axes [C,1,1] — the 2D analog of the same collapse (behavior 4).
			name: "1x1 Conv2D, unit spatial + kernel axes",
			node: &onnx.NodeProto{
				Attribute: []*onnx.AttributeProto{
					{Name: "kernel_shape", Ints: []int64{1, 1}},
					{Name: "pads", Ints: []int64{0, 0, 0, 0}},
					{Name: "strides", Ints: []int64{1, 1}},
				},
			},
			shapes:   [][]int{{1, 2, 1, 1}, {1, 2, 1, 1}, {1}},
			backings: [][]float32{{3, 4}, {5, 6}, {1}},
			// 3*5 + 4*6 + 1 = 40
			expectedShape: tensor.Shape{1, 1, 1, 1},
			expected:      []float32{40},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputs := []tensor.Tensor{
				ops.TensorWithBackingFixture(test.backings[0], test.shapes[0]...),
				ops.TensorWithBackingFixture(test.backings[1], test.shapes[1]...),
				nil,
			}
			if len(test.backings) == 3 {
				inputs[2] = ops.TensorWithBackingFixture(test.backings[2], test.shapes[2]...)
			}

			conv := convVersions[11]()
			err := conv.Init(test.node)
			assert.Nil(t, err)

			res, err := conv.Apply(inputs)
			if err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}

			assert.Equal(t, test.expectedShape, res[0].Shape())
			assert.Equal(t, test.expected, res[0].Data())
		})
	}
}

func onesF32(n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = 1
	}

	return out
}
