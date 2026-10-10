//go:build linux

package miner

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// L3Bytes is the sum of the L3 caches of every instance (sysfs). 0 = unknown.
//
// The instances are told apart by `id` (if that is missing, by the list of sharing CPUs). Virtual
// machines report the topology inconsistently (for instance 16 instances with different ids, each
// "shared" by every CPU) — better to overestimate the L3 and leave more threads than to slow the miner
// down by half.
func L3Bytes() uint64 {
	seen := map[string]bool{}
	var total uint64
	dirs, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cache/index3")
	for _, d := range dirs {
		key, err := os.ReadFile(filepath.Join(d, "id"))
		if err != nil {
			key, err = os.ReadFile(filepath.Join(d, "shared_cpu_list"))
		}
		if err != nil || seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		size, err := os.ReadFile(filepath.Join(d, "size"))
		if err != nil {
			continue
		}
		total += parseSize(strings.TrimSpace(string(size)))
	}
	return total
}

func parseSize(s string) uint64 {
	mul := uint64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mul, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mul, s = 1<<20, strings.TrimSuffix(s, "M")
	}
	v, _ := strconv.ParseUint(s, 10, 64)
	return v * mul
}

// CoreOrder returns the CPUs in this order: first one logical CPU from every physical core, then the SMT
// siblings. The miner's threads are pinned in this order — with fewer threads than logical CPUs each one
// therefore gets a whole core.
func CoreOrder() []int {
	n := runtime.NumCPU()
	first, rest := []int{}, []int{}
	seen := map[string]bool{}
	for cpu := 0; cpu < n; cpu++ {
		sib, err := os.ReadFile("/sys/devices/system/cpu/cpu" + strconv.Itoa(cpu) + "/topology/thread_siblings_list")
		key := strings.TrimSpace(string(sib))
		if err != nil {
			key = strconv.Itoa(cpu)
		}
		if seen[key] {
			rest = append(rest, cpu)
			continue
		}
		seen[key] = true
		first = append(first, cpu)
	}
	sort.Ints(first)
	sort.Ints(rest)
	return append(first, rest...)
}

// pin pins the current goroutine to an OS thread and that thread to one CPU.
func pin(cpu int) {
	runtime.LockOSThread()
	var set unix.CPUSet
	set.Set(cpu)
	_ = unix.SchedSetaffinity(0, &set)
}
