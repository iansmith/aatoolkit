package gonnx

import (
	"testing"

	"github.com/advancedclimatesystems/gonnx/ops"
	"github.com/stretchr/testify/assert"
)

func TestResolveOpset(t *testing.T) {
	_, err := ResolveOpset(13)
	assert.Nil(t, err)
}

func TestResolveOpsetNotSupported(t *testing.T) {
	opset, err := ResolveOpset(6)
	assert.Nil(t, opset)
	assert.Equal(t, ops.ErrUnsupportedOpsetVersion, err)
}

func TestResolveOpset16(t *testing.T) {
	// The lift to opset 16 must open the whole 14–16 range, not just the ceiling.
	for _, v := range []int64{14, 15, 16} {
		opset, err := ResolveOpset(v)
		assert.Nil(t, err)
		assert.NotNil(t, opset)
	}
}

func TestResolveOpsetUnsupported(t *testing.T) {
	opset, err := ResolveOpset(17)
	assert.Nil(t, opset)
	assert.Equal(t, ops.ErrUnsupportedOpsetVersion, err)
}
