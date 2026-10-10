package chain

import "math/big"

// MESS — scoring deep reorgs (the Ethereum Classic model, ECIP-1100).
//
// The older the fork point from our chain is, the more work a competing branch has to have for the
// node to accept it. Shallow reorgs (ordinary operation) pass practically without a penalty;
// rewriting half a day of history is impossible without 31 times the work.
//
// Careful, it is a **subjective** rule: every node decides by what it saw first. With two almost
// perfectly balanced partitions there is a risk of a permanent bifurcation — which is why MESS
// sits on top of normalized chainwork and only beyond the point where reorgs are rare anyway.
// Exchanges need this documented (the security notes §3).

const (
	// messXCap is the cap on the fork's age (≈ 7 h). Beyond it the penalty is constant.
	messXCap int64 = 25132
	// messHeight sets the curve's height; 3840 = 128 × (15 × 2), the same as in ECIP-1100.
	messHeight int64 = 3840
	// messScale is the scale the numerator is expressed in (128 = the neutral value).
	messScale int64 = 128
)

// messRequirement returns the fraction num/den = how many times the work a competing branch has to
// have.
//
// numerator(x) = 128 + (3x²·xcap − 2x³)·H / xcap³, all in integers.
func messRequirement(ageSeconds int64) (num, den *big.Int) {
	x := ageSeconds
	if x < 0 {
		x = 0
	}
	if x > messXCap {
		x = messXCap
	}
	bx := big.NewInt(x)
	cap1 := big.NewInt(messXCap)
	cap3 := new(big.Int).Exp(cap1, big.NewInt(3), nil)

	// 3x²·xcap − 2x³
	x2 := new(big.Int).Mul(bx, bx)
	term := new(big.Int).Mul(x2, cap1)
	term.Mul(term, big.NewInt(3))
	x3 := new(big.Int).Mul(x2, bx)
	term.Sub(term, new(big.Int).Mul(x3, big.NewInt(2)))
	term.Mul(term, big.NewInt(messHeight))

	num = new(big.Int).Mul(big.NewInt(messScale), cap3)
	num.Add(num, term)
	den = cap3
	return num, den
}

// MessMultiplier returns the required work multiple as a float — only for logging and
// documentation. The decision uses AcceptsReorg, which computes in integers.
func MessMultiplier(ageSeconds int64) float64 {
	num, den := messRequirement(ageSeconds)
	r := new(big.Rat).SetFrac(num, new(big.Int).Mul(den, big.NewInt(messScale)))
	f, _ := r.Float64()
	return f
}

// AcceptsReorg says whether the node should switch to a competing branch.
//
// localWork and proposedWork are the **normalized** chainworks from the common ancestor (not from
// the genesis), ageSeconds is the fork's age: the time of our head minus the time of the common
// ancestor.
//
// With equal work, what we already have wins — a node does not switch for free.
func AcceptsReorg(localWork, proposedWork *big.Int, ageSeconds int64) bool {
	if localWork == nil || proposedWork == nil {
		return false
	}
	num, den := messRequirement(ageSeconds)

	// accept if: proposed × 128 × den > num × local
	left := new(big.Int).Mul(proposedWork, big.NewInt(messScale))
	left.Mul(left, den)
	right := new(big.Int).Mul(num, localWork)
	return left.Cmp(right) > 0
}
