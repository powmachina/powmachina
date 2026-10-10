package miner

import (
	"testing"
	"time"
)

// The ratio of work to silence has to match the given fraction: at 20 %, after a 10 ms kernel it waits
// 40 ms, so the card is free 80 % of the time.
func TestDutyPause(t *testing.T) {
	for _, tc := range []struct {
		work time.Duration
		duty float64
		want time.Duration
	}{
		{10 * time.Millisecond, 0.2, 40 * time.Millisecond},
		{10 * time.Millisecond, 0.5, 10 * time.Millisecond},
		{10 * time.Millisecond, 0.75, 10 * time.Millisecond / 3},
		{10 * time.Millisecond, 1, 0},  // full speed — no waiting
		{10 * time.Millisecond, 0, 0},  // not set
		{0, 0.2, 0},                    // the kernel took no time
		{10 * time.Millisecond, -1, 0}, // nonsense
	} {
		got := dutyPause(tc.work, tc.duty)
		// Rounding to nanoseconds does not matter here, the ratio does.
		if diff := got - tc.want; diff > time.Microsecond || diff < -time.Microsecond {
			t.Fatalf("dutyPause(%v, %v) = %v, expected %v", tc.work, tc.duty, got, tc.want)
		}
	}
}
