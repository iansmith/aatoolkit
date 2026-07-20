package ops

import (
	"gorgonia.org/tensor"
)

// Activation is an activation function.
type Activation func(n tensor.Tensor) (tensor.Tensor, error)

// activations maps strings to the activation function. This is
// used by operators like LSTM, GRU and RNN.
var activations = map[string]Activation{
	"tanh":    Tanh,
	"sigmoid": Sigmoid,
	"relu":    ReLU,
}

func GetActivation(activation string) (Activation, error) {
	if a, ok := activations[activation]; ok {
		return a, nil
	}

	return nil, ErrActivationNotImplemented(activation)
}

// Tanh performs the tanh operation on a tensor.
func Tanh(X tensor.Tensor) (tensor.Tensor, error) {
	return tensor.Tanh(X)
}

// Sigmoid performs the sigmoid operation on a tensor.
//
// It is computed as 0.5*(1 + tanh(x/2)), which is algebraically identical to the
// textbook 1/(1+exp(-x)) but numerically stable: it exponentiates only through
// tanh, whose implementation stays finite for every input. The direct form is not
// safe here -- gorgonia's float32 tensor.Exp overflows to NaN/garbage once its
// argument exceeds ~88 (exp(89)=NaN, exp(90)=-9.3e-39), so 1/(1+exp(-x)) returned
// NaN for x < -88. Recurrent operators feed strongly-negative gate pre-activations
// into Sigmoid once their state magnitude grows, so that NaN poisoned the LSTM cell
// state on long real-audio runs of the Silero VAD model (SOP-89).
func Sigmoid(X tensor.Tensor) (tensor.Tensor, error) {
	typedHalf, err := GetValueAsTensorType(0.5, X.Dtype())
	if err != nil {
		return nil, err
	}

	halfX, err := tensor.Mul(X, typedHalf)
	if err != nil {
		return nil, err
	}

	tanhHalfX, err := tensor.Tanh(halfX)
	if err != nil {
		return nil, err
	}

	typedOne, err := GetValueAsTensorType(1.0, X.Dtype())
	if err != nil {
		return nil, err
	}

	onePlusTanh, err := tensor.Add(tanhHalfX, typedOne)
	if err != nil {
		return nil, err
	}

	return tensor.Mul(onePlusTanh, typedHalf)
}

// ReLU performs the ReLU operation on a tensor.
func ReLU(X tensor.Tensor) (tensor.Tensor, error) {
	typedZero, err := GetValueAsTensorType(0.0, X.Dtype())
	if err != nil {
		return nil, err
	}

	comparison, err := tensor.Gt(X, typedZero, tensor.AsSameType())
	if err != nil {
		return nil, err
	}

	return tensor.Mul(X, comparison)
}
