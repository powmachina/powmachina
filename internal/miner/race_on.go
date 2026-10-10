//go:build race

package miner

// raceEnabled says the binary is built with -race. Tests that measure hashing speed cannot be measured
// under the instrumentation — it is orders of magnitude slower.
const raceEnabled = true
