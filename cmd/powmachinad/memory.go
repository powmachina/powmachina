package main

import "log"

// autoFullMemory is the memory the auto mode wants free before it builds the full DAG for a
// sync: the DAG (~1 GiB), RandomX's dataset (2.1 GiB) and the rest of the node peak at ~4.7 GiB.
// A machine with less — a laptop, a small VM — would have the node killed by the system halfway
// through the build, so it syncs light instead (slower over a long history, a few hundred MiB).
const autoFullMemory = 6 << 30

// dagModeFor settles the auto mode against the memory available (avail, known = false when the
// system does not say): auto stays auto only when the full DAG fits.
func dagModeFor(mode dagMode, avail uint64, known bool) dagMode {
	if mode != dagAuto || !known || avail >= autoFullMemory {
		return mode
	}
	log.Printf("DAG: auto mode, but only %.1f GiB of memory is available and the full DAG needs ~5 GiB — staying light (-dag=full to force it)",
		float64(avail)/(1<<30))
	return dagLight
}
