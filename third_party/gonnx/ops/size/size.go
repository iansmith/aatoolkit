package size

import (
	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"gorgonia.org/tensor"
)

var sizeTypeConstraints = [][]tensor.Dtype{ops.AllTypes}

// Size represents the ONNX size operator.
type Size struct {
	ops.BaseOperator
}

// newSize creates a new size operator.
func newSize(version int, typeConstraints [][]tensor.Dtype) ops.Operator {
	return &Size{
		BaseOperator: ops.NewBaseOperator(
			version,
			1,
			1,
			typeConstraints,
			"size",
		),
	}
}

// Init initializes the size operator.
func (s *Size) Init(*onnx.NodeProto) error {
	return nil
}

// Apply the size operator. It returns a scalar int64 tensor holding the total
// number of elements of the input tensor.
func (s *Size) Apply(inputs []tensor.Tensor) ([]tensor.Tensor, error) {
	size := int64(inputs[0].Shape().TotalSize())
	out := tensor.New(tensor.FromScalar(size))

	return []tensor.Tensor{out}, nil
}
