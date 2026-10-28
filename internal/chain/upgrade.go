package chain

import "fmt"

// Fork-aware warnings.
//
// A node can only follow the consensus rules its own binary carries. A fork decided after the
// binary was built is not in its table at all (forks.go) — the one thing the node still sees is
// the signal bit miners set in the header version. When a bit this binary does not know is carried
// by a large part of the recent blocks, the network is getting ready for a change this node will
// not follow, and the honest thing is to say so loudly rather than drift onto an abandoned branch
// in silence.
//
// This is a WARNING, never an automatic stop. The header version is chosen by whoever mines the
// block, so any miner — a minority, an attacker — can set an unknown bit. A rule that let that
// stop or degrade a node would hand a part of the hashrate a switch to turn other people's nodes
// off. So the thresholds here are deliberately high and the only effect is a message: the node
// keeps enforcing the rules it has, and the operator decides what to do about the warning.

const (
	// unknownForkNoticePct: an unknown fork bit carried by at least this percent of the counted
	// blocks is worth a quiet heads-up.
	unknownForkNoticePct = 10
	// unknownForkWarnPct: carried by this percent it is a loud warning — a change this software
	// does not implement is close to being what most of the network mines.
	unknownForkWarnPct = 75
)

// UpgradeWarning is one reason the node may be running outdated software. It is advisory: the node
// never changes the rules it enforces because of it.
type UpgradeWarning struct {
	Severity string `json:"severity"` // "notice" or "warning"
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// Warnings reports the consensus changes the network is signalling that this binary does not know.
// It reads only the fork signalling already counted over the recent blocks (ForkStatus); it makes
// no network of its own and trusts no peer. An empty result means nothing unknown is being
// signalled above the notice threshold — not that the node is necessarily current.
func (st ForkStatus) Warnings() []UpgradeWarning {
	if st.Window == 0 {
		return nil
	}
	var out []UpgradeWarning
	for _, f := range st.Forks {
		if f.Known {
			continue // the binary has this rule; its bit is nothing to warn about
		}
		sig := 0
		for _, n := range f.Signalling {
			sig += n
		}
		if sig == 0 {
			continue
		}
		pct := sig * 100 / st.Window
		switch {
		case pct >= unknownForkWarnPct:
			out = append(out, UpgradeWarning{
				Severity: "warning",
				Code:     "unknown-fork",
				Message: fmt.Sprintf("the network is adopting a consensus change this software does not implement "+
					"(fork bit %d, %d%% of the last %d blocks). You are probably running outdated software — update "+
					"before it activates, or this node may be left on an abandoned branch.", uint8(f.Fork), pct, st.Window),
			})
		case pct >= unknownForkNoticePct:
			out = append(out, UpgradeWarning{
				Severity: "notice",
				Code:     "unknown-fork",
				Message: fmt.Sprintf("an unknown fork bit %d is being signalled by %d%% of the last %d blocks; a "+
					"consensus change this software does not implement may be coming — check for an update.",
					uint8(f.Fork), pct, st.Window),
			})
		}
	}
	return out
}
