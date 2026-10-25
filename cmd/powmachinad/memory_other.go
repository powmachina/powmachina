//go:build !linux && !windows

package main

// availableMemory: unknown here, so the auto mode stays as it is.
func availableMemory() (uint64, bool) { return 0, false }
