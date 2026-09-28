package eval

import "math"

// wilsonZ95 is the z-score for a 95% Wilson score interval
// (Φ⁻¹(0.975)).
const wilsonZ95 = 1.959963984540054

// wilsonInterval returns the 95% Wilson score interval for a binomial
// proportion k/n (design §4.10: "Wilson intervals on every rate"). The
// Wilson interval (rather than the naive normal approximation) stays
// inside [0,1] and behaves sensibly at k==0 or k==n, both of which are
// routine here (a rule that never flags anything in a small fixture; a
// tier cut every positive clears).
//
// n <= 0 returns (0, 0) — callers must not call this with n <= 0; Rate's
// own constructor (newRate) guards this by only ever calling it when
// n > 0.
func wilsonInterval(k, n int) (lo, hi float64) {
	if n <= 0 {
		return 0, 0
	}
	p := float64(k) / float64(n)
	z := wilsonZ95
	z2 := z * z
	denom := 1 + z2/float64(n)
	center := p + z2/(2*float64(n))
	margin := z * math.Sqrt(p*(1-p)/float64(n)+z2/(4*float64(n)*float64(n)))
	lo = (center - margin) / denom
	hi = (center + margin) / denom
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return lo, hi
}
