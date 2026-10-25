package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// availableMemory is what the system can give the node: MemAvailable, or less when the node runs
// in a container with a memory limit (cgroup v2, then v1).
func availableMemory() (uint64, bool) {
	avail, ok := memAvailable()
	if !ok {
		return 0, false
	}
	if limit, ok := cgroupRoom(); ok && limit < avail {
		avail = limit
	}
	return avail, true
}

func memAvailable() (uint64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			return kb << 10, err == nil
		}
	}
	return 0, false
}

// cgroupRoom is the container's limit minus what it uses already; false without a limit.
func cgroupRoom() (uint64, bool) {
	read := func(path string) (uint64, bool) {
		b, err := os.ReadFile(path)
		if err != nil {
			return 0, false
		}
		v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		return v, err == nil
	}
	limit, ok := read("/sys/fs/cgroup/memory.max") // "max" does not parse: no limit
	used, _ := read("/sys/fs/cgroup/memory.current")
	if !ok {
		limit, ok = read("/sys/fs/cgroup/memory/memory.limit_in_bytes")
		used, _ = read("/sys/fs/cgroup/memory/memory.usage_in_bytes")
		if !ok || limit >= 1<<60 { // v1 writes a huge number for no limit
			return 0, false
		}
	}
	if used >= limit {
		return 0, true
	}
	return limit - used, true
}
