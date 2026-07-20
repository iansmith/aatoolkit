package ifop

import (
	"testing"

	"github.com/advancedclimatesystems/gonnx/onnx"
	"github.com/stretchr/testify/assert"
)

// If is executed by the graph runner, never by Apply. Calling Apply directly must
// error rather than silently returning a wrong (empty) result — that would mask a
// runner that failed to short-circuit on the If node.
func TestIfApplyIsNeverCalledDirectly(t *testing.T) {
	op := &If{}

	assert.Nil(t, op.Init(&onnx.NodeProto{}))

	out, err := op.Apply(nil)
	assert.Nil(t, out)
	assert.Equal(t, ErrApplyCalled, err)
}
