package main

import "log"

// autoFullMemory is the memory the auto mode wants free before it builds the full DAG for a
// sync: the DAG (~1 GiB), RandomX's dataset (2.1 GiB) and the rest of the node peak at ~4.7 GiB.
// A machine with less — a laptop, a small VM — would have the node killed by the system halfway
// through the build, so it syncs light instead (slower over a long history, a few hundred MiB).
const autoFullMemory = 6 << 30

// dagModeFor settles the auto mode against the memory available (avail, known = false when the
// system does not say). Where the full DAG fits, the node keeps it at the tip too and verifies
// every share of every block; a light node checks only a sample of them (the Sharepool spec §7),
// which is a trust model, not something a node should fall into because it was left on the default.
func dagModeFor(mode dagMode, avail uint64, known bool) dagMode {
	if (mode != dagAuto && mode != dagSync) || !known {
		if mode == dagAuto {
			return dagSync // the system does not say: full while catching up, light at the tip
		}
		return mode
	}
	if mode == dagSync && avail >= autoFullMemory {
		return mode
	}
	if avail >= autoFullMemory {
		log.Printf("DAG: auto mode, %.1f GiB of memory is available — full, the node verifies every share (-dag=light to save the memory)",
			float64(avail)/(1<<30))
		return dagFull
	}
	log.Printf("DAG: %s mode, but only %.1f GiB of memory is available and the full DAG needs ~5 GiB — staying light (-dag=full to force it)",
		mode, float64(avail)/(1<<30))
	return dagLight
}
