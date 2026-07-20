package pad

import "github.com/advancedclimatesystems/gonnx/ops"

// Pad-11 moved pads and constant_value from attributes to inputs; Pad-13 only
// widened the type constraints. Opsets 1 and 2 use the attribute form and are
// deliberately not registered: a model declaring them resolves no operator and
// fails loudly with ErrUnknownOperatorType rather than silently mispadding.
var padVersions = ops.OperatorVersions{
	11: ops.NewOperatorConstructor(newPad, 11, padTypeConstraints),
	13: ops.NewOperatorConstructor(newPad, 13, padTypeConstraints),
}

func GetVersions() ops.OperatorVersions {
	return padVersions
}
