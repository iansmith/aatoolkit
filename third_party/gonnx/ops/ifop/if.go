// Package ifop registers the ONNX If control-flow operator. Go reserves the
// identifier "if", so the package is named ifop while the registry key stays "If".
//
// If is NOT a normal Apply operator: its behavior depends on subgraph attributes
// (then_branch / else_branch) and the surrounding tensor scope, neither of which
// Apply receives. The graph runner (model.go) executes If directly and never calls
// Apply — this type exists only so opset resolution and the conformance harness
// discover the operator. Apply therefore returns an error if it is ever reached.
package ifop

import (
	"errors"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/advancedclimatesystems/gonnx/ops"
	"gorgonia.org/tensor"
)

// ErrApplyCalled is returned if If.Apply is invoked directly. The graph runner is
// expected to execute If via its subgraphs and never dispatch to Apply.
var ErrApplyCalled = errors.New("if: If is executed by the graph runner, not by Apply")

var ifTypeConstraints = [][]tensor.Dtype{{tensor.Bool}}

// If represents the ONNX If operator. It is a registry placeholder only; the graph
// runner executes If nodes.
type If struct {
	ops.BaseOperator
}

// newIf creates a new If operator.
func newIf(version int, typeConstraints [][]tensor.Dtype) ops.Operator {
	return &If{
		BaseOperator: ops.NewBaseOperator(
			version,
			1,
			1,
			typeConstraints,
			"if",
		),
	}
}

// Init is a no-op: the graph runner reads the If node's subgraph attributes itself.
func (o *If) Init(*onnx.NodeProto) error {
	return nil
}

// Apply always errors. If is executed by the graph runner, which reads the
// then_branch/else_branch subgraphs and the enclosing scope; Apply sees neither and
// must never be the execution path for an If node.
func (o *If) Apply([]tensor.Tensor) ([]tensor.Tensor, error) {
	return nil, ErrApplyCalled
}
