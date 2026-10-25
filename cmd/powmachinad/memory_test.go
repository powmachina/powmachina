package main

import "testing"

// The auto mode builds the full DAG only where it fits: on a small machine the system would kill
// the node halfway through the build (a 4 GB VM: "The node stopped" in the wallet).
func TestAutoModeStaysLightWithoutTheMemory(t *testing.T) {
	cases := []struct {
		mode  dagMode
		avail uint64
		known bool
		want  dagMode
	}{
		{dagAuto, 3 << 30, true, dagLight},
		{dagAuto, autoFullMemory, true, dagAuto},
		{dagAuto, 0, false, dagAuto}, // the system does not say: as before
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
