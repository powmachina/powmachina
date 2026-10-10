package p2p

import (
	"testing"
	"time"
)

// The median offset has to survive a peer with a nonsensical time — that is exactly what a median is for.
func TestClockSkewMedianAndWarning(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var c ClockSkew

	if _, n := c.Median(); n != 0 {
		t.Fatal("an empty collector should have zero samples")
	}
	if _, off := c.Off(); off {
		t.Fatal("without samples there should be no warning")
	}

	// Five peers report a time 90 s ahead → our clock is behind.
	for i := 0; i < 5; i++ {
		c.Add(now.Add(90*time.Second).Unix(), now)
	}
	med, n := c.Median()
	if n != 5 || med != 90*time.Second {
		t.Fatalf("the median is %v from %d samples, expected 90s from 5", med, n)
	}
	if _, off := c.Off(); !off {
		t.Fatal("90 s above the threshold should have warned")
	}

	// One peer in 1970 does not spoil the median — it is discarded.
	c.Add(0, now)
	if med, _ := c.Median(); med != 90*time.Second {
		t.Fatalf("a nonsensical time changed the median to %v", med)
	}

	// Once enough peers with the right time are added, the warning disappears.
	for i := 0; i < 20; i++ {
		c.Add(now.Unix(), now)
	}
	if med, off := c.Off(); off {
		t.Fatalf("after balancing out there should be no warning (median %v)", med)
	}

	// The sample cap keeps memory in check.
	for i := 0; i < 500; i++ {
		c.Add(now.Unix(), now)
	}
	if _, n := c.Median(); n != ClockSamples {
		t.Fatalf("expected a cap of %d samples, got %d", ClockSamples, n)
	}
}
