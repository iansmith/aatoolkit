package gonnx

import (
	"testing"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/stretchr/testify/assert"
	"gorgonia.org/tensor"
)

func TestModel(t *testing.T) {
	tests := []struct {
		path     string
		input    Tensors
		expected Tensors
		err      error
	}{
		{
			"./sample_models/onnx_models/mlp.onnx",
			tensorsFixture(
				[]string{"data_input"},
				[][]int{{2, 3}},
				[][]float32{rangeFloat(6)},
			),
			tensorsFixture(
				[]string{"preds"},
				[][]int{{2, 2}},
				[][]float32{{-0.056310713, -1.1901507, -1.5961288, -3.3445296}},
			),
			nil,
		},
		{
			"./sample_models/onnx_models/mlp.onnx",
			tensorsFixture(
				[]string{"data_input"},
				[][]int{{2, 4, 2}},
				[][]float32{rangeFloat(16)},
			),
			nil,
			ErrInvalidShape([]onnx.Dim{{IsDynamic: true, Name: "batch_size", Size: 0}, {IsDynamic: false, Name: "", Size: 3}}, []int{2, 4, 2}),
		},
		{
			"./sample_models/onnx_models/mlp.onnx",
			tensorsFixture(
				[]string{"unknown_input"},
				[][]int{{2, 3}},
				[][]float32{rangeFloat(6)},
			),
			nil,
			ErrModel("tensor: %v not found", "data_input"),
		},
		{
			"./sample_models/onnx_models/gru.onnx",
			tensorsFixture(
				[]string{"data_input", "init_hidden"},
				[][]int{{1, 30, 3}, {1, 1, 5}},
				[][]float32{rangeFloat(90), rangeZeros(5)},
			),
			tensorsFixture(
				[]string{"preds", "hidden_out"},
				[][]int{{1, 30, 5}, {1, 1, 5}},
				[][]float32{expectedGruPredsOut(), expectedGruHiddenOut()},
			),
			nil,
		},
		{
			"./sample_models/onnx_models/scaler.onnx",
			tensorsFixture(
				[]string{"X"},
				[][]int{{2, 3}},
				[][]float32{{1.0, 10.0, 100.0, 1.5, 13.0, 120.0}},
			),
			tensorsFixture(
				[]string{"variable"},
				[][]int{{2, 3}},
				[][]float32{
					{-0.10153462, -0.15617376, -0.73914559, 1.67532117, 1.48365074, 1.91419756},
				},
			),
			nil,
		},
		{
			"./sample_models/onnx_models/ndm.onnx",
			tensorsFixture(
				[]string{"sensor_input", "setpoint_input"},
				[][]int{{1, 18, 4}, {1, 1}},
				[][]float32{rangeFloat(72), rangeFloat(1)},
			),
			tensorsFixture(
				[]string{"optimal_supply_temp"},
				[][]int{{1, 1}},
				[][]float32{{0.89400756}},
			),
			nil,
		},
	}

	for _, test := range tests {
		model, err := NewModelFromFile(test.path)
		assert.Nil(t, err)

		outputs, err := model.Run(test.input)

		assert.Equal(t, test.err, err)

		if test.expected == nil {
			assert.Nil(t, outputs)
		} else {
			for outputName := range test.expected {
				expectedTensor := test.expected[outputName]
				actualTensor := outputs[outputName]
				assert.InDeltaSlice(t, expectedTensor.Data(), actualTensor.Data(), 0.00001)
			}
		}
	}
}

func TestModelIOUtil(t *testing.T) {
	model, err := NewModelFromFile("./sample_models/onnx_models/mlp.onnx")
	assert.Nil(t, err)

	expectedInputShapes := onnx.Shapes{
		"data_input": []onnx.Dim{
			{IsDynamic: true, Name: "batch_size", Size: 0},
			{IsDynamic: false, Name: "", Size: 3},
		},
	}

	assert.Equal(t, []string{"data_input"}, model.InputNames())
	assert.Equal(t, expectedInputShapes, model.InputShapes())

	expectedOutputShapes := onnx.Shapes{
		"preds": []onnx.Dim{
			{IsDynamic: true, Name: "batch_size", Size: 0},
			{IsDynamic: false, Name: "", Size: 2},
		},
	}

	assert.Equal(t, []string{"preds"}, model.OutputNames())
	assert.Equal(t, expectedOutputShapes, model.OutputShapes())
	assert.Equal(t, expectedOutputShapes["preds"], model.OutputShape("preds"))

	assert.Equal(
		t,
		[]string{"layer1.weight", "layer1.bias", "layer2.weight", "layer2.bias"},
		model.ParamNames(),
	)

	assert.True(t, model.hasInput("data_input"))
	assert.False(t, model.hasInput("fail"))
}

func TestInputDimSize(t *testing.T) {
	model, err := NewModelFromFile("./sample_models/onnx_models/mlp.onnx")
	assert.Nil(t, err)

	dimSize, err := model.InputDimSize("data_input", 1)
	assert.Nil(t, err)
	assert.Equal(t, 3, dimSize)
}

func TestInputDimSizeInvalidInput(t *testing.T) {
	model, err := NewModelFromFile("./sample_models/onnx_models/mlp.onnx")
	assert.Nil(t, err)

	_, err = model.InputDimSize("swagger", 0)

	assert.Equal(t, ErrModel("input %v does not exist", "swagger"), err)
}

// --- If / subgraph fixtures ------------------------------------------------
//
// These build onnx.ModelProto values by hand so the graph runner can be exercised
// on control-flow structures that no vendored sample model provides. A branch that
// only needs to yield a constant is a graph with a single initializer whose name is
// the branch's declared output — running it binds that value with no nodes.

func boolTP(name string, val bool) *onnx.TensorProto {
	v := int32(0)
	if val {
		v = 1
	}

	return &onnx.TensorProto{
		Name:      name,
		DataType:  int32(onnx.TensorProto_BOOL),
		Dims:      []int64{1},
		Int32Data: []int32{v},
	}
}

func floatTP(name string, val float32) *onnx.TensorProto {
	return &onnx.TensorProto{
		Name:      name,
		DataType:  int32(onnx.TensorProto_FLOAT),
		Dims:      []int64{1},
		FloatData: []float32{val},
	}
}

// constBranch is a subgraph with no nodes whose declared output is an initializer,
// so running it yields that constant.
func constBranch(outName string, val float32) *onnx.GraphProto {
	return &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{floatTP(outName, val)},
		Output:      []*onnx.ValueInfoProto{{Name: outName}},
	}
}

func ifNode(cond string, outputs []string, thenG, elseG *onnx.GraphProto) *onnx.NodeProto {
	return &onnx.NodeProto{
		OpType: "If",
		Input:  []string{cond},
		Output: outputs,
		Attribute: []*onnx.AttributeProto{
			{Name: "then_branch", Type: onnx.AttributeProto_GRAPH, G: thenG},
			{Name: "else_branch", Type: onnx.AttributeProto_GRAPH, G: elseG},
		},
	}
}

func modelWith(g *onnx.GraphProto) *onnx.ModelProto {
	return &onnx.ModelProto{
		OpsetImport: []*onnx.OperatorSetIdProto{{Version: 13}},
		Graph:       g,
	}
}

// runModel builds and runs a hand-made model, failing the test fatally on any error
// so a nil result never reaches the caller's assertions.
func runModel(t *testing.T, g *onnx.GraphProto) Tensors {
	t.Helper()

	model, err := NewModel(modelWith(g))
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}

	out, err := model.Run(Tensors{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	return out
}

func TestRunIfSelectsThenBranch(t *testing.T) {
	g := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{boolTP("cond", true)},
		Node:        []*onnx.NodeProto{ifNode("cond", []string{"y"}, constBranch("t", 1.0), constBranch("e", 2.0))},
		Output:      []*onnx.ValueInfoProto{{Name: "y"}},
	}

	out := runModel(t, g)
	assert.Equal(t, []float32{1.0}, out["y"].Data())
}

func TestRunIfSelectsElseBranch(t *testing.T) {
	g := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{boolTP("cond", false)},
		Node:        []*onnx.NodeProto{ifNode("cond", []string{"y"}, constBranch("t", 1.0), constBranch("e", 2.0))},
		Output:      []*onnx.ValueInfoProto{{Name: "y"}},
	}

	out := runModel(t, g)
	assert.Equal(t, []float32{2.0}, out["y"].Data())
}

// nestedIf builds a chain of If nodes, each selecting its then-branch (which is the
// next If down) until a leaf constant. All conditions are true, so the leaf value
// must surface at the top.
func nestedIf(depth int, leaf float32) *onnx.GraphProto {
	if depth == 0 {
		return constBranch("leaf", leaf)
	}

	out := "out" + string(rune('0'+depth))

	return &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{boolTP("c", true)},
		Node:        []*onnx.NodeProto{ifNode("c", []string{out}, nestedIf(depth-1, leaf), constBranch("dead", -1.0))},
		Output:      []*onnx.ValueInfoProto{{Name: out}},
	}
}

func TestRunNestedIfDepth4(t *testing.T) {
	g := nestedIf(4, 42.0)

	out := runModel(t, g)
	assert.Equal(t, []float32{42.0}, out[g.GetOutput()[0].GetName()].Data())
}

// A branch that declares an output it never binds locally must resolve that name
// from the enclosing scope.
func TestRunIfSubgraphReadsOuterScope(t *testing.T) {
	readOuter := &onnx.GraphProto{Output: []*onnx.ValueInfoProto{{Name: "outer"}}}

	g := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{boolTP("cond", true), floatTP("outer", 7.0)},
		Node:        []*onnx.NodeProto{ifNode("cond", []string{"y"}, readOuter, constBranch("e", -1.0))},
		Output:      []*onnx.ValueInfoProto{{Name: "y"}},
	}

	out := runModel(t, g)
	assert.Equal(t, []float32{7.0}, out["y"].Data())
}

// A name bound inside a branch (here "shared", shadowing an outer initializer) must
// not overwrite the parent binding. The branch computes y from its local shared=99,
// but the parent's shared=1 must survive to the top-level output.
func TestRunIfSubgraphBindingsDoNotLeak(t *testing.T) {
	shadow := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{floatTP("shared", 99.0)},
		Output:      []*onnx.ValueInfoProto{{Name: "shared"}},
	}

	g := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{boolTP("cond", true), floatTP("shared", 1.0)},
		Node:        []*onnx.NodeProto{ifNode("cond", []string{"y"}, shadow, constBranch("e", -1.0))},
		Output:      []*onnx.ValueInfoProto{{Name: "y"}, {Name: "shared"}},
	}

	out := runModel(t, g)
	assert.Equal(t, []float32{99.0}, out["y"].Data(), "branch used its local shadow")
	assert.Equal(t, []float32{1.0}, out["shared"].Data(), "parent binding must not be overwritten by the branch")
}

// Else-branch scope resolution must work exactly like the then-branch: branch
// selection must not be a special-cased path that only wires scope for then.
func TestRunIfElseBranchReadsOuterScope(t *testing.T) {
	readOuter := &onnx.GraphProto{Output: []*onnx.ValueInfoProto{{Name: "outer"}}}

	g := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{boolTP("cond", false), floatTP("outer", 7.0)},
		Node:        []*onnx.NodeProto{ifNode("cond", []string{"y"}, constBranch("t", -1.0), readOuter)},
		Output:      []*onnx.ValueInfoProto{{Name: "y"}},
	}

	out := runModel(t, g)
	assert.Equal(t, []float32{7.0}, out["y"].Data())
}

// nestedElseIf mirrors nestedIf but nests through the else_branch with every
// condition false, so recursion must follow the else attribute too.
func nestedElseIf(depth int, leaf float32) *onnx.GraphProto {
	if depth == 0 {
		return constBranch("leaf", leaf)
	}

	out := "out" + string(rune('0'+depth))

	return &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{boolTP("c", false)},
		Node:        []*onnx.NodeProto{ifNode("c", []string{out}, constBranch("dead", -1.0), nestedElseIf(depth-1, leaf))},
		Output:      []*onnx.ValueInfoProto{{Name: out}},
	}
}

func TestRunNestedElseIfDepth4(t *testing.T) {
	g := nestedElseIf(4, 42.0)

	out := runModel(t, g)
	assert.Equal(t, []float32{42.0}, out[g.GetOutput()[0].GetName()].Data())
}

// DoD behavior 2, literal: a fresh name bound only inside a branch is absent from the
// parent scope. A later top-level node that reads it must fail to resolve it.
func TestRunIfInnerNameAbsentFromParent(t *testing.T) {
	branch := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{floatTP("tmp", 5.0)},
		Output:      []*onnx.ValueInfoProto{{Name: "tmp"}},
	}

	g := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{boolTP("cond", true)},
		Node: []*onnx.NodeProto{
			ifNode("cond", []string{"y"}, branch, constBranch("e", -1.0)),
			// "tmp" lived only inside the branch scope; reading it here must fail.
			{OpType: "Identity", Input: []string{"tmp"}, Output: []string{"z"}},
		},
		Output: []*onnx.ValueInfoProto{{Name: "z"}},
	}

	model, err := NewModel(modelWith(g))
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}

	_, err = model.Run(Tensors{})
	assert.ErrorContains(t, err, "tmp")
}

// Initializers declared inside a branch subgraph are available to that subgraph, and
// are collected once at construction: running the same model twice is identical.
func TestRunIfSubgraphInitializers(t *testing.T) {
	branch := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{floatTP("init_val", 5.0)},
		Output:      []*onnx.ValueInfoProto{{Name: "init_val"}},
	}

	g := &onnx.GraphProto{
		Initializer: []*onnx.TensorProto{boolTP("cond", true)},
		Node:        []*onnx.NodeProto{ifNode("cond", []string{"y"}, branch, constBranch("e", -1.0))},
		Output:      []*onnx.ValueInfoProto{{Name: "y"}},
	}

	model, err := NewModel(modelWith(g))
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}

	first, err := model.Run(Tensors{})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	assert.Equal(t, []float32{5.0}, first["y"].Data())

	second, err := model.Run(Tensors{})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	assert.Equal(t, first["y"].Data(), second["y"].Data())
}

// tensorsFixture creates Tensors with the given names shapes and backings. This is useful for
// providing a model with inputs and checking it's outputs.
func tensorsFixture(names []string, shapes [][]int, backing [][]float32) Tensors {
	res := make(Tensors, len(names))
	for i, name := range names {
		res[name] = tensor.New(
			tensor.WithShape(shapes[i]...),
			tensor.WithBacking(backing[i]),
		)
	}

	return res
}

func rangeFloat(size int) []float32 {
	res := make([]float32, size)
	for i := 0; i < size; i++ {
		res[i] = float32(i)
	}

	return res
}

func rangeZeros(size int) []float32 {
	res := make([]float32, size)
	for i := range res {
		res[i] = 0.0
	}

	return res
}

func expectedGruHiddenOut() []float32 {
	return []float32{0.45711097, 1, 0.9258882, -1, 1}
}

func expectedGruPredsOut() []float32 {
	return []float32{
		0.254439, 0.39027894, 0.12178477, 0.24339758, 0.39764592,
		0.3930065, 0.7781081, 0.41358948, 0.018615374, 0.9664475,
		0.43873432, 0.94687027, 0.5921345, -0.32937312, 0.99975467,
		0.45136297, 0.98945767, 0.6988256, -0.5221157, 0.99999934,
		0.45525378, 0.99754244, 0.76498276, -0.7068252, 1,
		0.45649233, 0.9993817, 0.8078227, -0.83324534, 1,
		0.45690063, 0.9998353, 0.8369877, -0.91363525, 1,
		0.45703846, 0.9999545, 0.8576527, -0.95835763, 1,
		0.45708582, 0.9999871, 0.8727655, -0.9809275, 1,
		0.45710227, 0.99999624, 0.8840895, -0.99153835, 1,
		0.457108, 0.9999989, 0.89273524, -0.99631274, 1,
		0.45711, 0.9999997, 0.899434, -0.99840873, 1,
		0.4571107, 0.9999999, 0.90468574, -0.9993168, 1,
		0.45711097, 1, 0.9088428, -0.9997075, 1,
		0.45711097, 1, 0.91215944, -0.99987495, 1,
		0.45711097, 1, 0.9148228, -0.9999466, 1,
		0.45711097, 1, 0.9169732, -0.99997723, 1,
		0.45711097, 1, 0.9187171, -0.9999903, 1,
		0.45711097, 1, 0.92013663, -0.9999958, 1,
		0.45711097, 1, 0.9212957, -0.9999982, 1,
		0.45711097, 1, 0.92224455, -0.9999992, 1,
		0.45711097, 1, 0.9230229, -0.9999997, 1,
		0.45711097, 1, 0.9236626, -0.9999999, 1,
		0.45711097, 1, 0.92418903, -0.99999994, 1,
		0.45711097, 1, 0.9246228, -1, 1,
		0.45711097, 1, 0.9249805, -1, 1,
		0.45711097, 1, 0.92527586, -1, 1,
		0.45711097, 1, 0.9255198, -1, 1,
		0.45711097, 1, 0.92572147, -1, 1,
		0.45711097, 1, 0.9258882, -1, 1,
	}
}
