package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// discoverInterfaces returns interface names from /sys/class/net/.
func discoverInterfaces() []string {
	return discoverInterfacesIn("")
}

// discoverInterfacesIn returns interface names, optionally inside a network namespace.
func discoverInterfacesIn(ns string) []string {
	var entries []os.DirEntry
	var err error

	if ns == "" {
		entries, err = os.ReadDir("/sys/class/net")
	} else {
		// Use ip netns exec to list interfaces in a namespace.
		out, cmdErr := runCmdInNS(ns, []string{"ls", "/sys/class/net"})
		if cmdErr != nil {
			return nil
		}
		var names []string
		for _, name := range strings.Fields(out) {
			if name != "" && name != "." && name != ".." {
				names = append(names, name)
			}
		}
		return names
	}

	if err != nil {
		return nil
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// discoverCgroupPaths finds cgroup stat files based on cgroup version.
// For v1: finds cpu.stat under /sys/fs/cgroup/cpu/ and memory.stat under /sys/fs/cgroup/memory/
// For v2: finds cpu.stat, memory.stat, memory.current, io.stat under /sys/fs/cgroup/
func discoverCgroupPaths(cgroupV int) []string {
	var paths []string

	if cgroupV == 2 {
		for _, name := range []string{"cpu.stat", "memory.stat", "memory.current", "io.stat"} {
			paths = append(paths, findFiles("/sys/fs/cgroup", name)...)
		}
	} else {
		paths = append(paths, findFiles("/sys/fs/cgroup/cpu", "cpu.stat")...)
		paths = append(paths, findFiles("/sys/fs/cgroup/memory", "memory.stat")...)
	}

	return paths
}

// findFiles walks a directory tree looking for files with a specific name.
func findFiles(root, name string) []string {
	var result []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if !d.IsDir() && d.Name() == name {
			result = append(result, path)
		}
		return nil
	})
	return result
}

// discoverQEMUPIDs returns PIDs of qemu-system processes.
func discoverQEMUPIDs() []int {
	out, err := runCmd([]string{"pgrep", "-x", "qemu-system"})
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(out) {
		if pid, err := strconv.Atoi(line); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// discoverQEMUThreads returns thread IDs for a given QEMU PID.
func discoverQEMUThreads(pid int) []int {
	taskDir := filepath.Join("/proc", strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	var tids []int
	for _, e := range entries {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, tid)
		}
	}
	return tids
}

// discoverQEMUKernelThreads finds kernel threads associated with a QEMU PID.
// These are separate processes (not in QEMU's task dir) whose comm contains
// the QEMU PID, e.g. "vhost-5756", "kvm-pit/5756".
func discoverQEMUKernelThreads(qemuPID int) []int {
	pidStr := strconv.Itoa(qemuPID)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var kthreadPIDs []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tid, err := strconv.Atoi(e.Name())
		if err != nil || tid == qemuPID {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if strings.Contains(name, pidStr) {
			kthreadPIDs = append(kthreadPIDs, tid)
		}
	}
	return kthreadPIDs
}

// discoverInterfaceStatFiles returns all files under /sys/class/net/<intf>/statistics/.
func discoverInterfaceStatFiles(intf string) []string {
	dir := filepath.Join("/sys/class/net", intf, "statistics")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths
}

// discoverInterfaceQueueFiles returns all files under /sys/class/net/<intf>/queues/.
func discoverInterfaceQueueFiles(intf string) []string {
	root := filepath.Join("/sys/class/net", intf, "queues")
	var paths []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	return paths
}

// detectCgroupVersion returns 2 if cgroup v2 is mounted, 1 otherwise.
func detectCgroupVersion() int {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return 2
	}
	return 1
}

// getConf runs getconf and returns the integer value.
func getConf(name string) int {
	out, err := runCmd([]string{"getconf", name})
	if err != nil {
		return 0
	}
	val, _ := strconv.Atoi(strings.TrimSpace(out))
	return val
}

// getHostname returns the system hostname.
func getHostname() string {
	h, _ := os.Hostname()
	return h
}
