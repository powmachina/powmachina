package p2p

import (
	"sort"
	"sync"
	"time"
)

// The machine's clock against the network.
//
// Consensus follows the **local** clock (`MaxFutureDrift` is 120 s) and that is how it should stay:
// if the time validation went by depended on the peers, the network could be shifted by whoever you
// connect to it. The time from the handshake is therefore only **diagnostics** — but important ones:
// a machine whose clock is two minutes off will not give its blocks to the network (it rejects them)
// and will reject foreign ones as "too far in the future". From the outside it looks like a network
// outage and a beginner has no way to work out that their clock is wrong.
//
// Bitcoin and Monero do the same: the median offset from the peers and a loud warning.

const (
	// ClockSamples is how many of the most recent peers go into the median.
	ClockSamples = 64
	// ClockWarnSkew is the offset from which the operator is told.
	// 60 s is half of MaxFutureDrift — nothing is on fire yet, but it is time to act.
	ClockWarnSkew = 60 * time.Second
	// ClockMinSamples is how many peers are needed for the median to make sense.
	ClockMinSamples = 5
)

// ClockSkew collects the offsets of the peers' time from our clock.
type ClockSkew struct {
	mu      sync.Mutex
	samples []time.Duration
}

// Add records the offset from a handshake: the peer's time minus ours.
func (c *ClockSkew) Add(peerUnix int64, now time.Time) {
	// Nonsensical values (a peer in 1970 or 2200) do not belong in the median — they would spoil it
	// and they are either bugs or somebody testing what we do.
	d := time.Unix(peerUnix, 0).Sub(now)
	if d > 24*time.Hour || d < -24*time.Hour {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, d)
	if len(c.samples) > ClockSamples {
		c.samples = c.samples[len(c.samples)-ClockSamples:]
	}
}

// Median returns the offset of our clock from the network and the number of samples.
func (c *ClockSkew) Median() (time.Duration, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.samples)
	if n == 0 {
		return 0, 0
	}
	sorted := append([]time.Duration(nil), c.samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[n/2], n
}

// Off says whether the machine's clock is off far enough that it has to be said.
func (c *ClockSkew) Off() (time.Duration, bool) {
	med, n := c.Median()
	if n < ClockMinSamples {
		return med, false
	}
	return med, med > ClockWarnSkew || med < -ClockWarnSkew
}
