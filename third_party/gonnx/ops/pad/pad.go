package pad

import (
	"reflect"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"gorgonia.org/tensor"
)

// padTypeConstraints lists the allowed types per input: data, pads, constant_value.
// pads is always int64; constant_value shares data's type.
var padTypeConstraints = [][]tensor.Dtype{
	ops.AllTypes,
	{tensor.Int64},
	ops.AllTypes,
}

// Supported padding modes. `wrap` (opset 19) is deliberately absent: it must fail
// loudly rather than silently pad some other way.
const (
	modeConstant = "constant"
	modeReflect  = "reflect"
	modeEdge     = "edge"
)

// Pad represents the ONNX pad operator.
type Pad struct {
	ops.BaseOperator

	mode string
}

// newPad creates a new pad operator.
func newPad(version int, typeConstraints [][]tensor.Dtype) ops.Operator {
	return &Pad{
		BaseOperator: ops.NewBaseOperator(
			version,
			2,
			3,
			typeConstraints,
			"pad",
		),
		mode: modeConstant,
	}
}

// Init initializes the pad operator. An absent or empty `mode` attribute means
// "constant", per the ONNX default.
func (p *Pad) Init(n *onnx.NodeProto) error {
	p.mode = modeConstant

	for _, attr := range n.GetAttribute() {
		if attr.GetName() != "mode" {
			return ops.ErrInvalidAttribute(attr.GetName(), p)
		}

		mode := string(attr.GetS())
		if mode == "" {
			continue
		}

		switch mode {
		case modeConstant, modeReflect, modeEdge:
			p.mode = mode
		default:
			return ops.ErrUnsupportedAttribute("mode", p)
		}
	}

	return nil
}

// Apply the pad operator. `pads` holds 2*rank int64s laid out as
// [begin_0..begin_{r-1}, end_0..end_{r-1}]. A negative entry crops that edge instead
// of padding it, which is why the source index below is simply `coord - begin`:
// cropping and padding are the same shift, and cropping therefore works in every mode.
func (p *Pad) Apply(inputs []tensor.Tensor) ([]tensor.Tensor, error) {
	data := inputs[0]
	inShape := data.Shape()
	rank := len(inShape)

	begins, ends, err := p.readPads(inputs[1], rank)
	if err != nil {
		return nil, err
	}

	outShape := make([]int, rank)

	for i := range rank {
		outShape[i] = inShape[i] + begins[i] + ends[i]
		if outShape[i] <= 0 {
			return nil, ops.ErrInvalidInput("pads crop axis to a non-positive size", p.BaseOperator)
		}
	}

	if err := p.checkPaddable(inShape, begins, ends); err != nil {
		return nil, err
	}

	fill, err := p.fillValue(inputs, data.Dtype())
	if err != nil {
		return nil, err
	}

	out := tensor.New(tensor.Of(data.Dtype()), tensor.WithShape(outShape...))

	// Walk every output coordinate with the tensor's own iterator; srcCoord reuses
	// one buffer per step.
	srcCoord := make([]int, rank)

	it := out.Iterator()
	// We cannot check the error here since it is a post statement so ignore the nolint errcheck here.
	// nolint errcheck
	for it.Reset(); !it.Done(); it.Next() {
		outCoord := it.Coord()

		var v any
		if p.sourceCoord(outCoord, begins, inShape, srcCoord) {
			v, err = data.At(srcCoord...)
			if err != nil {
				return nil, err
			}
		} else {
			v = fill
		}

		if err := out.SetAt(v, outCoord...); err != nil {
			return nil, err
		}
	}

	return []tensor.Tensor{out}, nil
}

// sourceCoord fills srcCoord with the input coordinate that feeds outCoord and
// reports whether it lands inside the input. It returns false only in constant
// mode, where an out-of-range axis means the output cell takes the fill value.
func (p *Pad) sourceCoord(outCoord, begins []int, inShape tensor.Shape, srcCoord []int) bool {
	for i := range srcCoord {
		j, ok := p.sourceIndex(outCoord[i]-begins[i], inShape[i])
		if !ok {
			return false
		}

		srcCoord[i] = j
	}

	return true
}

// readPads splits the pads tensor into per-axis begin and end amounts.
//
// The shape is checked before Data() is touched: gorgonia panics when Data() is
// called on a zero-length tensor, so an empty pads input would crash here rather
// than reach the length check below.
func (p *Pad) readPads(pads tensor.Tensor, rank int) (begins, ends []int, err error) {
	if rank < 1 {
		return nil, nil, ops.ErrInvalidInput("data must have rank >= 1", p.BaseOperator)
	}

	shape := pads.Shape()
	if len(shape) != 1 || shape.TotalSize() != 2*rank {
		return nil, nil, ops.ErrInvalidInput("pads must be a 1D tensor holding exactly 2*rank entries", p.BaseOperator)
	}

	raw, ok := pads.Data().([]int64)
	if !ok {
		return nil, nil, ops.ErrInvalidInput("pads must be a 1D int64 tensor", p.BaseOperator)
	}

	begins = make([]int, rank)
	ends = make([]int, rank)

	for i := range rank {
		begins[i] = int(raw[i])
		ends[i] = int(raw[i+rank])
	}

	return begins, ends, nil
}

// checkPaddable rejects pads whose padded cells have no source to read from.
//
// Negative pads crop first, so every bound is against the axis size that SURVIVES the
// opposite side's crop, not the original size. Bounding against the original instead
// reads an element that was cropped away and returns a plausible but wrong tensor
// where ONNX errors.
//
//   - reflect mirrors about the surviving edges, so the largest legal pad is
//     surviving-1.
//   - edge replicates a surviving element, so it needs at least one to replicate.
//   - constant reads no source at all, so it has nothing to check.
//
// Both rules were checked against onnxruntime over every (n, begin, end) for n <= 5.
func (p *Pad) checkPaddable(inShape tensor.Shape, begins, ends []int) error {
	if p.mode == modeConstant {
		return nil
	}

	for i := range begins {
		surviving := inShape[i] + min(begins[i], 0) + min(ends[i], 0)

		switch p.mode {
		case modeReflect:
			if max(begins[i], 0) >= surviving || max(ends[i], 0) >= surviving {
				return ops.ErrInvalidInput("reflect pad must be smaller than the axis that survives cropping", p.BaseOperator)
			}
		case modeEdge:
			if surviving < 1 {
				return ops.ErrInvalidInput("edge pad has no surviving element to replicate", p.BaseOperator)
			}
		}
	}

	return nil
}

// sourceIndex maps an (already begin-shifted) index onto the input axis. It reports
// false only in constant mode, where an out-of-range index means "use the fill value".
func (p *Pad) sourceIndex(j, n int) (int, bool) {
	if j >= 0 && j < n {
		return j, true
	}

	switch p.mode {
	case modeEdge:
		return min(max(j, 0), n-1), true
	case modeReflect:
		// Mirror once, without repeating the edge. j is already out of range here
		// (the in-range case returned above), and checkReflectable bounds every pad
		// below n, so a single reflection off the near or far edge always lands in
		// [0, n) -- no loop needed.
		if j < 0 {
			return -j, true
		}

		return 2*(n-1) - j, true
	default:
		return 0, false
	}
}

// fillValue returns the constant to pad with. It is only meaningful in constant mode;
// in every other mode the padded cells come from the input, so the value is ignored.
func (p *Pad) fillValue(inputs []tensor.Tensor, dtype tensor.Dtype) (any, error) {
	zero := reflect.Zero(dtype.Type).Interface()

	// Outside constant mode the fill is never read (sourceIndex never reports
	// out-of-range), so return the zero without inspecting constant_value. This
	// also skips the type check below: ONNX ignores constant_value in non-constant
	// modes, so a mismatched dtype there must not be an error.
	if p.mode != modeConstant || len(inputs) < 3 || inputs[2] == nil {
		return zero, nil
	}

	// Check the shape before Data(): gorgonia panics on a zero-length tensor, the same
	// trap readPads guards against.
	if inputs[2].Shape().TotalSize() != 1 {
		return nil, ops.ErrInvalidInput("constant_value must be a scalar", p.BaseOperator)
	}

	// constant_value is rank-0, so Data() yields a bare value rather than a slice.
	// Accept a 1-element rank-1 tensor too; both spellings appear in the wild.
	v := reflect.ValueOf(inputs[2].Data())
	if v.Kind() == reflect.Slice {
		v = v.Index(0)
	}

	if v.Type() != dtype.Type {
		return nil, ops.ErrInvalidInput("constant_value must have the same type as data", p.BaseOperator)
	}

	return v.Interface(), nil
}
