package slice

import (
	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"gorgonia.org/tensor"
)

var sliceTypeConstraints = [][]tensor.Dtype{
	ops.AllTypes,
	{tensor.Int32, tensor.Int64},
	{tensor.Int32, tensor.Int64},
	{tensor.Int32, tensor.Int64},
	{tensor.Int32, tensor.Int64},
}

const (
	MinSliceInputs = 3
	MaxSliceInputs = 5
)

// Slice represents the ONNX slice operator.
type Slice struct {
	ops.BaseOperator
}

// newSlice creates a new slice operator.
func newSlice(version int, typeConstraints [][]tensor.Dtype) ops.Operator {
	return &Slice{
		BaseOperator: ops.NewBaseOperator(
			version,
			MinSliceInputs,
			MaxSliceInputs,
			typeConstraints,
			"slice",
		),
	}
}

// Init initializes the slice operator.
func (s *Slice) Init(*onnx.NodeProto) error {
	return nil
}

// Apply applies the slice operator.
func (s *Slice) Apply(inputs []tensor.Tensor) ([]tensor.Tensor, error) {
	data := inputs[0]

	starts, err := intSliceInput(inputs[1], nil)
	if err != nil {
		return nil, err
	}

	ends, err := intSliceInput(inputs[2], nil)
	if err != nil {
		return nil, err
	}

	axes, err := intSliceInput(inputs[3], getDefaultAxes(len(starts)))
	if err != nil {
		return nil, err
	}

	steps, err := intSliceInput(inputs[4], getDefaultSteps(len(starts)))
	if err != nil {
		return nil, err
	}

	rank := len(data.Shape())
	if err := validateAxes(axes, rank); err != nil {
		return nil, err
	}

	// Slice every axis via gather-by-index rather than gorgonia's Dense.Slice. gorgonia
	// drops an axis sliced to length 1 and cannot express a negative step, whereas ONNX
	// keeps every sliced axis (output rank == input rank) and reverses on a negative step.
	// Routing both signs through sliceEachAxis preserves the rank ONNX/onnxruntime expect.
	out, err := sliceEachAxis(data, starts, ends, axes, steps)
	if err != nil {
		return nil, err
	}

	return []tensor.Tensor{out}, nil
}

// intSliceInput parses an optional int-slice input, returning fallback when the input
// is absent (nil). starts and ends are mandatory, so their fallback is never used.
func intSliceInput(input tensor.Tensor, fallback []int) ([]int, error) {
	if input == nil {
		return fallback, nil
	}

	return ops.AnyToIntSlice(ops.IfScalarToSlice(input.Data()))
}

// validateAxes rejects any axis outside the tensor's rank, so a malformed slice errors
// loudly rather than indexing out of bounds and panicking (in constructSlices or sliceAxis).
func validateAxes(axes []int, rank int) error {
	for _, axis := range axes {
		if axis < -rank || axis >= rank {
			return ops.ErrAxisOutOfRange(rank, rank, axis)
		}
	}

	return nil
}

// sliceEachAxis applies the slice one axis at a time. ONNX slice axes are distinct
// and each start/end clamps against its own axis size, which slicing other axes does
// not change, so sequential per-axis application is equivalent to the batched form —
// and it lets the negative-step (reverse) axes be handled individually.
func sliceEachAxis(data tensor.Tensor, starts, ends, axes, steps []int) (tensor.Tensor, error) {
	rank := len(data.Shape())
	out := data

	for i, axis := range axes {
		if axis < 0 {
			axis += rank
		}

		var err error

		out, err = sliceAxis(out, axis, starts[i], ends[i], steps[i])
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

// sliceAxis slices a single axis into the concrete ONNX index sequence and gathers it.
// Both signs go through gatherAxis rather than gorgonia's Slice: gorgonia drops a
// sliced-to-length-1 axis and errors on an empty range, whereas ONNX keeps the axis and
// treats an empty range as a valid empty slice — and gorgonia cannot express a negative
// step at all.
func sliceAxis(t tensor.Tensor, axis, start, end, step int) (tensor.Tensor, error) {
	dim := t.Shape()[axis]

	if start < 0 {
		start += dim
	}

	if end < 0 {
		end += dim
	}

	idxs := make([]int, 0)

	if step > 0 {
		start = clampInt(start, 0, dim)
		end = clampInt(end, 0, dim)

		for i := start; i < end; i += step {
			idxs = append(idxs, i)
		}
	} else {
		// Per ONNX, for a negative step start clamps to [0, dim-1] and end to [-1, dim-1]
		// (the INT64_MIN "to the beginning" sentinel lands on -1 after the += dim above).
		start = clampInt(start, 0, dim-1)
		end = clampInt(end, -1, dim-1)

		for i := start; i > end; i += step {
			idxs = append(idxs, i)
		}
	}

	return gatherAxis(t, axis, idxs)
}

// gatherAxis returns a new tensor whose given axis is re-indexed by idxs, copying via
// At/SetAt so it is correct for any dtype. An empty idxs yields an empty axis rather
// than a panic.
func gatherAxis(t tensor.Tensor, axis int, idxs []int) (tensor.Tensor, error) {
	inShape := t.Shape()

	outShape := make(tensor.Shape, len(inShape))
	copy(outShape, inShape)
	outShape[axis] = len(idxs)

	out := tensor.New(tensor.WithShape(outShape...), tensor.Of(t.Dtype()))

	total := outShape.TotalSize()
	coord := make([]int, len(outShape))
	inCoord := make([]int, len(inShape))

	for n := 0; n < total; n++ {
		copy(inCoord, coord)
		inCoord[axis] = idxs[coord[axis]]

		val, err := t.At(inCoord...)
		if err != nil {
			return nil, err
		}

		if err := out.SetAt(val, coord...); err != nil {
			return nil, err
		}

		for k := len(coord) - 1; k >= 0; k-- {
			coord[k]++
			if coord[k] < outShape[k] {
				break
			}

			coord[k] = 0
		}
	}

	return out, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}

	if v > hi {
		return hi
	}

	return v
}

// constructSlice constructs a list with tensor.Slice objects. The list is initializes with nils.
// The axes parameter determines at which indices tensor.Slice objects are placed.
func constructSlices(starts, ends, steps, axes []int, nTotalSlices int) []tensor.Slice {
	slices := make([]tensor.Slice, nTotalSlices)
	for i := 0; i < nTotalSlices; i++ {
		slices[i] = nil
	}

	for i, ax := range axes {
		if ax < 0 {
			ax = nTotalSlices + ax
		}

		slices[ax] = ops.NewSlicer(starts[i], ends[i], steps[i])
	}

	return slices
}

// getDefaultAxes returns the default axes parameter. By default the slices are in natural order.
func getDefaultAxes(nSlices int) []int {
	axes := make([]int, nSlices)
	for i := 0; i < nSlices; i++ {
		axes[i] = i
	}

	return axes
}

// getDefaultSteps returns the default steps data. By default the steps are 1.
func getDefaultSteps(nSlices int) []int {
	steps := make([]int, nSlices)
	for i := 0; i < nSlices; i++ {
		steps[i] = 1
	}

	return steps
}
