package main

import "testing"

// The auto mode builds the full DAG only where it fits: on a small machine the system would kill
// the node halfway through the build (a 4 GB VM: "The node stopped" in the wallet). Where it fits,
// the node stays full at the tip and verifies every share.
func TestAutoModeStaysLightWithoutTheMemory(t *testing.T) {
	cases := []struct {
		mode  dagMode
		avail uint64
		known bool
		want  dagMode
	}{
		{dagAuto, 3 << 30, true, dagLight},
		{dagAuto, autoFullMemory, true, dagFull},
		{dagAuto, 0, false, dagSync}, // the system does not say: full while catching up, light at the tip
		{dagSync, 3 << 30, true, dagLight},
		{dagSync, autoFullMemory, true, dagSync},
		{dagSync, 0, false, dagSync},
		{dagFull, 1 << 30, true, dagFull},
		{dagLight, 64 << 30, true, dagLight},
	}
	for _, c := range cases {
		if got := dagModeFor(c.mode, c.avail, c.known); got != c.want {
			t.Errorf("%s with %d MiB (known %v): %s, want %s", c.mode, c.avail>>20, c.known, got, c.want)
		}
	}
	if avail, ok := availableMemory(); !ok || avail == 0 {
		t.Errorf("this system's available memory: %d, known %v", avail, ok)
	}
}
