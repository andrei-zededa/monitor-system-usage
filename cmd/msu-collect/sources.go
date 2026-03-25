package main

import (
	"fmt"
	"path/filepath"
)

// Source represents one data collection action.
type Source struct {
	Cmd     string   // canonical identifier, e.g. "cat /proc/stat"
	Section string   // "init", "A", or "B"
	Type    string   // "file" or "exec"
	Path    string   // file path (Type=="file")
	Args    []string // command args (Type=="exec")
	NS      string   // network namespace (empty for root)
}

func fileSource(section, path string) Source {
	return Source{
		Cmd:     "cat " + path,
		Section: section,
		Type:    "file",
		Path:    path,
	}
}

func execSource(section string, args ...string) Source {
	cmd := args[0]
	for _, a := range args[1:] {
		cmd += " " + a
	}
	return Source{
		Cmd:     cmd,
		Section: section,
		Type:    "exec",
		Args:    args,
	}
}

func fileSourceNS(section, ns, path string) Source {
	s := fileSource(section, path)
	s.NS = ns
	return s
}

func execSourceNS(section, ns string, args ...string) Source {
	s := execSource(section, args...)
	s.NS = ns
	return s
}

// staticASources returns the fixed A-section sources (not per-interface or per-NS).
func staticASources() []Source {
	return []Source{
		execSource("A", "ps", "auxwww"),
		fileSource("A", "/proc/interrupts"),
		fileSource("A", "/proc/softirqs"),
		fileSource("A", "/proc/net/dev"),
		fileSource("A", "/proc/net/softnet_stat"),
		fileSource("A", "/proc/net/netstat"),
		fileSource("A", "/proc/net/snmp"),
		fileSource("A", "/proc/net/snmp6"),
		fileSource("A", "/proc/net/sockstat"),
		execSource("A", "iptables", "-vnL"),
		execSource("A", "iptables", "-t", "nat", "-vnL"),
		execSource("A", "ip6tables", "-vnL"),
		execSource("A", "ip6tables", "-t", "nat", "-vnL"),
		fileSource("A", "/proc/sys/net/netfilter/nf_conntrack_count"),
		fileSource("A", "/proc/sys/net/netfilter/nf_conntrack_max"),
		execSource("A", "bridge", "fdb", "show"),
		execSource("A", "bridge", "vlan", "show"),
		execSource("A", "ip", "route", "show", "table", "all"),
	}
}

// interfaceSources returns per-interface A-section sources.
func interfaceSources(intf, ns string) []Source {
	section := "A"
	mk := func(args ...string) Source {
		if ns != "" {
			return execSourceNS(section, ns, args...)
		}
		return execSource(section, args...)
	}
	mkFile := func(path string) Source {
		if ns != "" {
			return fileSourceNS(section, ns, path)
		}
		return fileSource(section, path)
	}

	sources := []Source{
		mk("ip", "-d", "-s", "addr", "show", intf),
		mk("ethtool", "-k", intf),
		mk("ethtool", "-l", intf),
		mk("ethtool", "-c", intf),
		mk("ethtool", "-g", intf),
		mk("ethtool", "-S", intf),
		mk("ethtool", "--phy-statistics", intf),
		mk("tc", "-s", "qdisc", "show", "dev", intf),
		mk("tc", "-s", "class", "show", "dev", intf),
	}

	// Per-interface sysfs statistics files
	if ns == "" {
		for _, path := range discoverInterfaceStatFiles(intf) {
			sources = append(sources, mkFile(path))
		}
		for _, path := range discoverInterfaceQueueFiles(intf) {
			sources = append(sources, mkFile(path))
		}
	}
	// For NS interfaces, sysfs discovery would need ip netns exec which is complex;
	// the shell script does it via find inside the namespace. We handle commands
	// above which cover the important data. Sysfs files in namespaces are omitted
	// for now as they require running find inside the namespace.

	return sources
}

// nsSources returns per-namespace A-section sources (network files + iptables).
func nsSources(ns string) []Source {
	return []Source{
		fileSourceNS("A", ns, "/proc/net/dev"),
		fileSourceNS("A", ns, "/proc/net/softnet_stat"),
		fileSourceNS("A", ns, "/proc/net/netstat"),
		fileSourceNS("A", ns, "/proc/net/snmp"),
		fileSourceNS("A", ns, "/proc/net/snmp6"),
		fileSourceNS("A", ns, "/proc/net/sockstat"),
		fileSourceNS("A", ns, "/proc/softirqs"),
		execSourceNS("A", ns, "iptables", "-vnL"),
		execSourceNS("A", ns, "iptables", "-t", "nat", "-vnL"),
		execSourceNS("A", ns, "ip6tables", "-vnL"),
		execSourceNS("A", ns, "ip6tables", "-t", "nat", "-vnL"),
	}
}

// staticBSources returns the fixed B-section sources.
func staticBSources() []Source {
	return []Source{
		fileSource("B", "/proc/stat"),
		fileSource("B", "/proc/meminfo"),
		fileSource("B", "/proc/loadavg"),
		fileSource("B", "/proc/net/softnet_stat"),
		fileSource("B", "/proc/pressure/cpu"),
		fileSource("B", "/proc/vmstat"),
		fileSource("B", "/proc/pressure/memory"),
		fileSource("B", "/proc/diskstats"),
		fileSource("B", "/proc/pressure/io"),
	}
}

// cgroupSources returns B-section sources for discovered cgroup paths.
func cgroupSources(paths []string) []Source {
	var sources []Source
	for _, path := range paths {
		sources = append(sources, fileSource("B", path))
	}
	return sources
}

// qemuSources returns B-section sources for a QEMU process.
func qemuSources(pid int) []Source {
	p := fmt.Sprintf("/proc/%d", pid)
	sources := []Source{
		fileSource("B", filepath.Join(p, "stat")),
		fileSource("B", filepath.Join(p, "statm")),
		fileSource("B", filepath.Join(p, "status")),
		fileSource("B", filepath.Join(p, "wchan")),
		fileSource("B", filepath.Join(p, "sched")),
		fileSource("B", filepath.Join(p, "schedstat")),
	}
	// In-process threads (vcpu threads like "CPU 0/KVM").
	for _, tid := range discoverQEMUThreads(pid) {
		taskDir := filepath.Join(p, "task", fmt.Sprintf("%d", tid))
		sources = append(sources,
			fileSource("B", filepath.Join(taskDir, "stat")),
			fileSource("B", filepath.Join(taskDir, "comm")),
		)
	}
	// Associated kernel threads (separate PIDs like vhost-<pid>, kvm-pit/<pid>).
	for _, ktPID := range discoverQEMUKernelThreads(pid) {
		kp := fmt.Sprintf("/proc/%d", ktPID)
		sources = append(sources,
			fileSource("B", filepath.Join(kp, "stat")),
			fileSource("B", filepath.Join(kp, "comm")),
		)
	}
	return sources
}

// initSources returns one-time sources collected before the loop.
func initSources() []Source {
	var sources []Source
	for _, intf := range discoverInterfaces() {
		sources = append(sources, execSource("init", "ethtool", "-i", intf))
	}
	return sources
}
