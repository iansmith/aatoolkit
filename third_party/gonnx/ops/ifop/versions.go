package ifop

import "github.com/advancedclimatesystems/gonnx/ops"

var ifVersions = ops.OperatorVersions{
	1:  ops.NewOperatorConstructor(newIf, 1, ifTypeConstraints),
	11: ops.NewOperatorConstructor(newIf, 11, ifTypeConstraints),
	13: ops.NewOperatorConstructor(newIf, 13, ifTypeConstraints),
}

// GetVersions returns the registered versions of the If operator.
func GetVersions() ops.OperatorVersions {
	return ifVersions
}
