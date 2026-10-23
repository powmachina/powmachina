package chain

import (
	"time"

	"powmachina/internal/mh"
)

// The lane rotation rule: a block must not have the same lane as both of its predecessors.
//
// Without this rule, 51 % in one domain is enough for a reorg — an attacker would pick the lane
// with the lowest hashrate and rewrite the chain alone. With it they need **both** domains, that
// is, CPU and GPU.
//
// With two lanes it matters all the more because lane C is RandomX: RandomX hashrate can be
// rented (Monero has orders of magnitude more, NiceHash sells it). But an attacker with rented
// CPUs makes at most two blocks in a row and then has to deliver lane G — and that they cannot rent.
//
// recent are the lanes of the preceding blocks, the newest first. A shorter list (the start of the
// chain) is fine.
func LaneAllowed(recent []mh.Lane, lane mh.Lane) bool {
	if len(recent) < MaxSameLaneRun {
		return true
	}
	for i := 0; i < MaxSameLaneRun; i++ {
		if recent[i] != lane {
			return true
		}
	}
	return false
}

// LaneAllowedAt adds a liveness fallback: when the network stalls — this block comes more
// than LaneRotationRelief after its parent — the rule is waived for this block. Without
// that, one lane dying would stop the chain for good.
//
// The gap is measured against the **parent block**, not against median time past. MTP is
// the median of eleven blocks, so it trails the tip by roughly six of them (~12 minutes at
// a 120 s block, three at 30 s against a relief of five). Measured against MTP, a perfectly healthy
// chain satisfies the relief most of the time: the trail plus an ordinary gap between blocks.
// The rule that is the whole point of two lanes would therefore be switched off much of the
// time, and one lane could run dozens of blocks in a row.
//
// Both times come from block headers, so every node gets the same answer forever. Using
// the local clock here would make the same block valid for one node and invalid for
// another, and historical validation would always see the relief.
//
// Faking the gap buys an attacker little: from ForkLaneDifficulty on a block is not dated
// before its parent and at most 30 s ahead of the real clock, so of a claimed 5-minute gap at
// least 270 s are real. (Before the fork the bounds were the median time past and 120 s, and
// most of the gap could be made up.)
func LaneAllowedAt(recent []mh.Lane, lane mh.Lane, blockTime, parentTime time.Time) bool {
	if blockTime.Sub(parentTime) > LaneRotationRelief {
		return true
	}
	return LaneAllowed(recent, lane)
}

// AllowedLanes returns the lanes a node may currently hand out templates for. A
// decentralised pool needs this so it does not offer work that cannot become a block
// (the sharechain note §5).
//
// `blockTime` is when the block being built would be timestamped (now, for a template);
// `parentTime` is the timestamp of the current tip.
func AllowedLanes(recent []mh.Lane, blockTime, parentTime time.Time) []mh.Lane {
	// A header carries whole seconds. Asked with the clock's sub-second part, this would open a
	// lane 300.3 s after the parent, the block built on that answer would say 300 s, and 300 is
	// not more than the relief — every block of that job is refused, for as long as the job lives.
	blockTime = time.Unix(blockTime.Unix(), 0)
	out := make([]mh.Lane, 0, len(mh.Lanes))
	for _, l := range mh.Lanes {
		if LaneAllowedAt(recent, l, blockTime, parentTime) {
			out = append(out, l)
		}
	}
	return out
}
