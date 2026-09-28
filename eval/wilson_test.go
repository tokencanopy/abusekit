package eval

import (
	"math"
	"testing"
)

// TestWilsonInterval_KnownAnswers checks wilsonInterval against
// hand-computed values from the standard closed-form Wilson score
// interval formula (not this package's own implementation — these are
// independently derived so a bug shared between the test and the code
// can't hide):
//
//	denom  = 1 + z²/n
//	center = p + z²/(2n)
//	margin = z * sqrt(p(1-p)/n + z²/(4n²))
//	lo,hi  = (center ∓ margin) / denom
//
// with z = 1.959964 (the 95% two-sided normal quantile).
//
// The n=20,k=10 case (p=0.5) is the textbook 95% Wilson interval example
// commonly cited as approximately (0.299, 0.701); the n=10,k=0 case is
// the boundary case commonly cited as approximately (0, 0.278).
func TestWilsonInterval_KnownAnswers(t *testing.T) {
	tests := []struct {
		name      string
		k, n      int
		wantLo    float64
		wantHi    float64
		tolerance float64
	}{
		{"10/20 (p=0.5)", 10, 20, 0.2992, 0.7008, 0.001},
		{"0/10 (boundary)", 0, 10, 0.0, 0.2775, 0.001},
		{"10/10 (boundary)", 10, 10, 0.7225, 1.0, 0.001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lo, hi := wilsonInterval(tt.k, tt.n)
			if math.Abs(lo-tt.wantLo) > tt.tolerance {
				t.Errorf("lo = %.4f, want ~%.4f (+/- %.4f)", lo, tt.wantLo, tt.tolerance)
			}
			if math.Abs(hi-tt.wantHi) > tt.tolerance {
				t.Errorf("hi = %.4f, want ~%.4f (+/- %.4f)", hi, tt.wantHi, tt.tolerance)
			}
			if lo < 0 || hi > 1 || lo > hi {
				t.Errorf("interval (%.4f, %.4f) is not a valid probability interval", lo, hi)
			}
		})
	}
}

func TestWilsonInterval_ZeroN(t *testing.T) {
	lo, hi := wilsonInterval(0, 0)
	if lo != 0 || hi != 0 {
		t.Errorf("wilsonInterval(0, 0) = (%v, %v), want (0, 0)", lo, hi)
	}
}

// TestNewRate_Undefined checks Rate's "no evidence" contract: N==0 must
// report Defined=false and every numeric field at its zero value, never
// a stray 0% that could be misread as "measured and zero".
func TestNewRate_Undefined(t *testing.T) {
	r := newRate(0, 0)
	if r.Defined {
		t.Errorf("newRate(0, 0).Defined = true, want false")
	}
	if r.Value != 0 || r.WilsonLow != 0 || r.WilsonHigh != 0 {
		t.Errorf("newRate(0, 0) = %+v, want every field zero", r)
	}
}
