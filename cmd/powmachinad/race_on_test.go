//go:build race

package main

// raceEnabled says the test binary is built with -race, where a long scenario costs several times
// as much.
const raceEnabled = true
