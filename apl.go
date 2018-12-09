package main

import (
	"fmt"
	"os"

	"github.com/ktye/iv/apl"
	"github.com/ktye/iv/apl/numbers"
	"github.com/ktye/iv/apl/operators"
	"github.com/ktye/iv/apl/primitives"
)

// StartApl starts a new apl interpreter and returns the callback for an apl function call.
//
// Example: echo 1 2 3 | ./awk '{print(apl("+/" $0))}'
// Output: 6
//
// TODO: user functions cannot receive or return arrays.
// There is also no way to access global variables to look up arrays.
func startApl() func(string, complex128, complex128) (complex128, error) {
	a := apl.New(os.Stdout)
	numbers.Register(a)
	primitives.Register(a)
	operators.Register(a)

	return func(expr string, L, R complex128) (complex128, error) {
		a.Assign("L", numbers.Complex(L))
		a.Assign("R", numbers.Complex(R))

		var z complex128
		p, err := a.Parse(expr)
		if err != nil {
			return z, err
		}

		if len(p) != 1 {
			return z, fmt.Errorf("apl call must evaluate to a single expression")
		}
		v, err := p[0].Eval(a)
		if err != nil {
			return z, fmt.Errorf("apl: %s", err)
		}
		if n, ok := v.(numbers.Integer); ok == true {
			return complex(float64(n), 0), nil
		} else if n, ok := v.(numbers.Float); ok == true {
			return complex(n, 0), nil
		} else if n, ok := v.(numbers.Complex); ok {
			return complex128(n), nil
		} else {
			return z, fmt.Errorf("apl: result has type %T", v)
		}
	}
}
