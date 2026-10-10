//go:build !linux

package miner

import "runtime"

// L3Bytes cannot be determined outside Linux without further dependencies.
func L3Bytes() uint64 { return 0 }

// CoreOrder outside Linux: the natural order.
func CoreOrder() []int {
	out := make([]int, runtime.NumCPU())
	for i := range out {
		out[i] = i
	}
	return out
}

func pin(int) { runtime.LockOSThread() }
