package gonnx

import (
	"archive/zip"
	"io"
	"os"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"google.golang.org/protobuf/proto"
	"gorgonia.org/tensor"
)

// Tensors is a map with tensors.
type Tensors map[string]tensor.Tensor

// Model defines a model that can be used for inference.
type Model struct {
	mp         *onnx.ModelProto
	parameters Tensors
	Opset      Opset

	// subgraphParams holds the initializers of every If subgraph, keyed by the
	// subgraph pointer. Collected once in NewModel (never per Run) and seeded into a
	// subgraph's scope when the runner enters it.
	subgraphParams map[*onnx.GraphProto]Tensors
}

// scope is a lexical tensor scope. Lookups walk up the parent chain (a subgraph sees
// outer-scope tensors), while binds write only the local map (a name bound inside a
// subgraph shadows the outer one and does not leak to the parent). The child scope
// holds a parent pointer rather than a copy of the parent's map.
type scope struct {
	tensors Tensors
	parent  *scope
}

func (s *scope) get(name string) (tensor.Tensor, bool) {
	if t, ok := s.tensors[name]; ok {
		return t, true
	}

	if s.parent != nil {
		return s.parent.get(name)
	}

	return nil, false
}

func (s *scope) set(name string, t tensor.Tensor) {
	s.tensors[name] = t
}

// NewModelFromFile creates a new model from a path to a file.
func NewModelFromFile(path string) (*Model, error) {
	bytesModel, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return NewModelFromBytes(bytesModel)
}

// NewModelFromZipFile creates a new model from a file in a zip archive.
func NewModelFromZipFile(file *zip.File) (*Model, error) {
	fc, err := file.Open()
	if err != nil {
		return nil, err
	}

	bytesModel, err := io.ReadAll(fc)
	if err != nil {
		return nil, err
	}

	return NewModelFromBytes(bytesModel)
}

// NewModelFromBytes creates a new model from a list of bytes.
func NewModelFromBytes(bytesModel []byte) (*Model, error) {
	mp, err := ModelProtoFromBytes(bytesModel)
	if err != nil {
		return nil, err
	}

	return NewModel(mp)
}

// NewModel creates a new model ready for inference given a path to an onnx file.
func NewModel(mp *onnx.ModelProto) (*Model, error) {
	params, err := mp.Graph.Params()
	if err != nil {
		return nil, err
	}

	opsetImports := mp.GetOpsetImport()

	var opsetID int64

	for i := 0; i < len(opsetImports); i++ {
		version := opsetImports[i].GetVersion()
		if version > opsetID {
			opsetID = version
		}
	}

	opset, err := ResolveOpset(opsetID)
	if err != nil {
		return nil, err
	}

	subgraphParams, err := collectSubgraphParams(mp.Graph)
	if err != nil {
		return nil, err
	}

	return &Model{
		mp:             mp,
		parameters:     params,
		Opset:          opset,
		subgraphParams: subgraphParams,
	}, nil
}

// collectSubgraphParams walks every subgraph reachable through node graph-attributes
// (the If then_branch / else_branch, recursively) and records each subgraph's own
// initializers as tensors, keyed by the subgraph pointer. Done once at construction so
// Run never rebuilds initializer tensors.
func collectSubgraphParams(g *onnx.GraphProto) (map[*onnx.GraphProto]Tensors, error) {
	result := make(map[*onnx.GraphProto]Tensors)

	var walk func(g *onnx.GraphProto) error

	walk = func(g *onnx.GraphProto) error {
		for _, n := range g.GetNode() {
			for _, attr := range n.GetAttribute() {
				sub := attr.GetG()
				if sub == nil {
					continue
				}

				params, err := sub.Params()
				if err != nil {
					return err
				}

				result[sub] = params

				if err := walk(sub); err != nil {
					return err
				}
			}
		}

		return nil
	}

	if err := walk(g); err != nil {
		return nil, err
	}

	return result, nil
}

// ModelProtoFromBytes creates an onnx.ModelProto based on a list of bytes.
func ModelProtoFromBytes(bytesModel []byte) (*onnx.ModelProto, error) {
	mp := &onnx.ModelProto{}
	if err := proto.Unmarshal(bytesModel, mp); err != nil {
		return nil, err
	}

	return mp, nil
}

// InputNames returns this models input names as defined by the model proto.
func (m *Model) InputNames() []string {
	return m.mp.Graph.InputNames()
}

// InputShapes returns the shapes for all input tensors.
func (m *Model) InputShapes() onnx.Shapes {
	return m.mp.Graph.InputShapes()
}

// InputDimSize returns the size of the input dimension given an input tensor.
func (m *Model) InputDimSize(input string, i int) (int, error) {
	if !m.hasInput(input) {
		return 0, ErrModel("input %v does not exist", input)
	}

	inputShape := m.mp.Graph.InputShapes()[input]

	if i >= len(inputShape) {
		return 0, ErrModel("input %v only has %d dimensions, but index %d was required", input, len(inputShape), i)
	}

	return int(inputShape[i].Size), nil
}

// OutputNames returns this models output names as defined by the model proto.
func (m *Model) OutputNames() []string {
	return m.mp.Graph.OutputNames()
}

// OutputShapes returns the shapes for all output tensors.
func (m *Model) OutputShapes() onnx.Shapes {
	return m.mp.Graph.OutputShapes()
}

// OutputShape returns the shape of a specific output tensors.
func (m *Model) OutputShape(output string) onnx.Shape {
	return m.mp.Graph.OutputShapes()[output]
}

// ParamNames returns this models parameter names as defined by the model proto.
func (m *Model) ParamNames() []string {
	return m.mp.Graph.ParamNames()
}

func (m *Model) hasInput(input string) bool {
	for _, inputName := range m.InputNames() {
		if inputName == input {
			return true
		}
	}

	return false
}

// Run builds and executes the computional graph of the network given the inputs.
func (m *Model) Run(inputs Tensors) (Tensors, error) {
	if err := m.validateShapes(inputs); err != nil {
		return nil, err
	}

	root := &scope{tensors: make(Tensors)}
	for inputName, inputTensor := range inputs {
		root.set(inputName, inputTensor)
	}

	for parameterName, parameterTensor := range m.parameters {
		root.set(parameterName, parameterTensor)
	}

	if err := m.runGraph(m.mp.Graph, root); err != nil {
		return nil, err
	}

	outputTensors := make(Tensors)
	for _, outputName := range m.OutputNames() {
		tensor, _ := root.get(outputName)
		outputTensors[outputName] = tensor
	}

	return outputTensors, nil
}

// runGraph executes every node of a graph within the given scope. An If node is
// executed directly (short-circuited before the opset lookup) so its subgraph
// attributes and the enclosing scope are available; its Apply is never invoked.
func (m *Model) runGraph(g *onnx.GraphProto, sc *scope) error {
	for _, n := range g.GetNode() {
		if n.GetOpType() == "If" {
			if err := m.runIf(n, sc); err != nil {
				return err
			}

			continue
		}

		op, ok := m.Opset[n.GetOpType()]
		if !ok {
			return ops.ErrUnknownOperatorType(n.GetOpType())
		}

		if err := m.applyOp(op(), n, sc); err != nil {
			return err
		}
	}

	return nil
}

// runIf executes an If node: it reads the boolean condition, runs the then_branch or
// else_branch subgraph in a child scope, and binds the selected branch's declared
// outputs to the If node's output names in the parent scope.
func (m *Model) runIf(n *onnx.NodeProto, sc *scope) error {
	inputs := n.GetInput()
	if len(inputs) != 1 {
		return ErrModel("If expects exactly 1 input (the condition), got %d", len(inputs))
	}

	condTensor, ok := sc.get(inputs[0])
	if !ok {
		return ErrModel("no tensor yet for name %v", inputs[0])
	}

	cond, err := scalarBool(condTensor)
	if err != nil {
		return err
	}

	branch, err := ifBranch(n, cond)
	if err != nil {
		return err
	}

	child := &scope{tensors: make(Tensors), parent: sc}
	for name, t := range m.subgraphParams[branch] {
		child.set(name, t)
	}

	if err := m.runGraph(branch, child); err != nil {
		return err
	}

	branchOutputs := branch.GetOutput()
	nodeOutputs := n.GetOutput()

	if len(branchOutputs) != len(nodeOutputs) {
		return ErrModel(
			"If branch declares %d outputs but the node has %d", len(branchOutputs), len(nodeOutputs),
		)
	}

	// Resolve every branch output from the child scope before binding any into the
	// parent: child.get can walk up to the parent, so writing as we go would let a
	// later lookup read a name an earlier iteration just bound in the parent.
	vals := make([]tensor.Tensor, len(nodeOutputs))

	for i := range nodeOutputs {
		branchName := branchOutputs[i].GetName()

		val, ok := child.get(branchName)
		if !ok {
			return ErrModel("no tensor yet for name %v", branchName)
		}

		vals[i] = val
	}

	for i, nodeOutput := range nodeOutputs {
		sc.set(nodeOutput, vals[i])
	}

	return nil
}

// ifBranch returns the then_branch subgraph when cond is true, else the else_branch.
func ifBranch(n *onnx.NodeProto, cond bool) (*onnx.GraphProto, error) {
	want := "else_branch"
	if cond {
		want = "then_branch"
	}

	for _, attr := range n.GetAttribute() {
		if attr.GetName() == want {
			g := attr.GetG()
			if g == nil {
				return nil, ErrModel("If attribute %v is not a graph", want)
			}

			return g, nil
		}
	}

	return nil, ErrModel("If node is missing the %v attribute", want)
}

// scalarBool reads a single boolean from a condition tensor. gorgonia exposes a rank-0
// tensor's Data() as a bare bool and a single-element tensor's as a []bool, so both are
// accepted (ONNX allows either for the If condition). Any other shape — higher rank, or
// zero/multiple elements — is a malformed condition and errors rather than reading a
// stray value. Checking the shape first also avoids Data() on a zero-length tensor.
func scalarBool(t tensor.Tensor) (bool, error) {
	if shape := t.Shape(); len(shape) > 1 || (len(shape) == 1 && shape[0] != 1) {
		return false, ErrModel("If condition must be a scalar or single-element bool, got shape %v", shape)
	}

	switch data := t.Data().(type) {
	case bool:
		return data, nil
	case []bool:
		if len(data) != 1 {
			return false, ErrModel("If condition must be a single boolean, got %d elements", len(data))
		}

		return data[0], nil
	default:
		return false, ErrModel("If condition must be a boolean tensor, got %T", t.Data())
	}
}

// applyOp applies the operation to the graph within the given scope.
func (m *Model) applyOp(op ops.Operator, n *onnx.NodeProto, sc *scope) error {
	if err := op.Init(n); err != nil {
		return err
	}

	inputTensors, err := getInputTensorsForNode(n.GetInput(), sc)
	if err != nil {
		return err
	}

	inputTensors, err = op.ValidateInputs(inputTensors)
	if err != nil {
		return err
	}

	outputTensors, err := op.Apply(inputTensors)
	if err != nil {
		return err
	}

	return setOutputTensorsOfNode(n.GetOutput(), outputTensors, sc)
}

// validateShapes validates if the tensors passed in have the same shape as the shapes defined
// by the onnx.Shapes.
func (m *Model) validateShapes(inputTensors Tensors) error {
	for name, shapeExpected := range m.InputShapes() {
		// If the input is a parameter, the user does not have to provide a tensor for it.
		if _, ok := m.parameters[name]; ok {
			continue
		}

		tensor, ok := inputTensors[name]
		if !ok {
			return ErrModel("tensor: %v not found", name)
		}

		shapeReceived := tensor.Shape()

		if len(shapeReceived) != len(shapeExpected) {
			return ErrInvalidShape(shapeExpected, shapeReceived)
		}

		for i, dim := range shapeExpected {
			// because the dimension is dynamic, it can have any size
			// and we do not have to check for it
			if dim.IsDynamic {
				continue
			}

			if dim.Size != int64(shapeReceived[i]) {
				return ErrInvalidShape(shapeExpected, shapeReceived)
			}
		}
	}

	return nil
}

func getInputTensorsForNode(names []string, sc *scope) ([]tensor.Tensor, error) {
	var inputTensors []tensor.Tensor

	for _, tensorName := range names {
		// An empty name can happen in between optional inputs, like:
		//   [<required_input>, <optional_input>, nil, <optional_input>]
		// In such a case, ONNX includes the name of the input in the node, and we need
		// to set a value (nil) for it, although it will not be used.
		if tensorName == "" {
			inputTensors = append(inputTensors, nil)
		} else if tensor, ok := sc.get(tensorName); ok {
			inputTensors = append(inputTensors, tensor)
		} else {
			return nil, ErrModel("no tensor yet for name %v", tensorName)
		}
	}

	return inputTensors, nil
}

func setOutputTensorsOfNode(
	names []string, outputTensors []tensor.Tensor, sc *scope,
) error {
	if len(names) != len(outputTensors) {
		return ErrModel("could not set output tensor")
	}

	for i, tensor := range outputTensors {
		sc.set(names[i], tensor)
	}

	return nil
}
