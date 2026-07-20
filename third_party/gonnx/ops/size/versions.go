package size

import "github.com/advancedclimatesystems/gonnx/ops"

var sizeVersions = ops.OperatorVersions{
	1:  ops.NewOperatorConstructor(newSize, 1, sizeTypeConstraints),
	13: ops.NewOperatorConstructor(newSize, 13, sizeTypeConstraints),
}

func GetVersions() ops.OperatorVersions {
	return sizeVersions
}
