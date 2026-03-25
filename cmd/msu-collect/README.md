# msu-collect

System monitoring data collector for EVE-OS. Replaces `script/monitor_system_usage.sh`
with a Go binary that writes structured CBOR output instead of ad-hoc text.

Designed to run inside an EVE-OS debug container (`eve enter debug`) for extended
periods. Data is flushed to disk every collection interval so that a system crash
loses at most one interval of data. Memory usage is bounded regardless of runtime
duration.

## Usage

```
msu-collect [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-interval` | `10` | Collection interval in seconds |
| `-flush-interval` | `6` | Flush to disk every N collection intervals (default: 6 = every 60s at 10s interval) |
| `-n` | (none) | Comma-separated list of network namespaces to also monitor |
| `-o` | stdout | Output file path (`.msu.cbor`). If omitted, writes to stdout |
| `-dump` | (none) | Dump a CBOR file to human-readable text and exit |
| `-version` | | Print version and exit |

### Examples

```sh
# Collect to a file, 10s interval:
msu-collect -o /persist/newlog/keepSentQueue/msu_data.msu.cbor

# Collect with network namespace monitoring:
msu-collect -n ns1,ns2 -o /persist/msu.cbor

# Inspect a CBOR file:
msu-collect -dump /persist/msu.cbor | less

# Typical EVE-OS deployment:
eve enter debug
apk add --no-cache musl-utils iproute2 ethtool
msu-collect -interval 10 -o /persist/newlog/keepSentQueue/msu.cbor &
```

## Output Format

CBOR (RFC 8949) sequence — concatenated self-contained CBOR items, one per
collected sample. The first item is a header record; all subsequent items are
sample records.

The `msu` analyzer (`cmd/msu`) auto-detects CBOR vs legacy text format and
can process both transparently.

### Header Record

Written once at the start. Contains system metadata:

| Field | Description |
|-------|-------------|
| `v` | Format version (currently 1) |
| `type` | Always `"header"` |
| `ts` | Start time (RFC 3339 UTC) |
| `msu_ver` | Collector version |
| `hz` | `CLK_TCK` — kernel clock ticks per second |
| `psz` | `PAGESIZE` in bytes |
| `cgroup_v` | Cgroup version (1 or 2) |
| `hostname` | System hostname |

### Sample Record

One per collected file or command execution:

| Field | Description |
|-------|-------------|
| `v` | Format version |
| `ts` | Collection time (RFC 3339 UTC, per-sample precision) |
| `seq` | Monotonic interval counter |
| `sec` | Section: `"init"`, `"A"`, or `"B"` |
| `cmd` | Canonical command identifier, e.g. `"cat /proc/stat"` |
| `ns` | Network namespace (omitted for root namespace) |
| `out` | Raw output, newline-joined string |
| `err` | Error message if collection failed (omitted on success) |

## Collection Cadence

The collector runs in a loop with two section types, matching the original
shell script's behavior:

- **B sections** run every interval (default 10s) — lightweight, high-frequency data
- **A sections** run every 3rd interval (~30s) — heavier, less frequent data
- **Init** runs once before the loop — static system information

Dynamic state (network interfaces, QEMU PIDs, cgroup paths) is re-discovered
at the start of each A section.

## Collected Data

### Init (one-time)

| Command | Purpose |
|---------|---------|
| `ethtool -i <intf>` | Driver info per network interface (driver name, version, bus info). Does not change at runtime. |

### A Section (every 3rd interval)

#### Process Listing

| Command | Purpose |
|---------|---------|
| `ps auxwww` | Full process listing with CPU%, MEM%, command lines. Provides general system overview and is used to identify QEMU processes and associated kernel threads. |

#### CPU / Interrupt Accounting

| Source | Purpose |
|--------|---------|
| `/proc/interrupts` | Per-CPU hardware interrupt counters by IRQ number. Shows interrupt distribution across CPUs. |
| `/proc/softirqs` | Per-CPU software interrupt counters by type (NET_RX, NET_TX, TIMER, SCHED, etc.). Key for diagnosing network processing bottlenecks. |

#### Network Stack

| Source | Purpose |
|--------|---------|
| `/proc/net/dev` | Per-interface packet/byte counters and error counts (RX/TX). |
| `/proc/net/softnet_stat` | Per-CPU packet processing counters: packets processed, drops, time_squeeze events. Hex-encoded, one row per CPU. Drops or squeezes indicate the CPU can't keep up with packet rate. |
| `/proc/net/netstat` | Extended TCP statistics (retransmits, SACKs, DSACK, fast retrans, etc.) and IP extension stats. |
| `/proc/net/snmp` | SNMP MIB counters: IP, ICMP, TCP, UDP aggregate statistics. |
| `/proc/net/snmp6` | IPv6 SNMP MIB counters. |
| `/proc/net/sockstat` | Socket usage summary: count of TCP, UDP, RAW sockets and memory usage. |

#### Firewall / Routing / Bridging

| Command | Purpose |
|---------|---------|
| `iptables -vnL` | IPv4 filter table rules with packet/byte counters. |
| `iptables -t nat -vnL` | IPv4 NAT table rules with counters. |
| `ip6tables -vnL` | IPv6 filter table rules with counters. |
| `ip6tables -t nat -vnL` | IPv6 NAT table rules with counters. |
| `bridge fdb show` | Bridge forwarding database (MAC address table). |
| `bridge vlan show` | VLAN configuration on bridge ports. |
| `ip route show table all` | All routing tables (main, local, custom). |

#### Connection Tracking

| Source | Purpose |
|--------|---------|
| `/proc/sys/net/netfilter/nf_conntrack_count` | Current number of tracked connections. |
| `/proc/sys/net/netfilter/nf_conntrack_max` | Maximum conntrack table size. Approaching the max causes packet drops. |

#### Per-Interface Details (for each interface in `/sys/class/net/`)

| Command | Purpose |
|---------|---------|
| `ip -d -s addr show <intf>` | Interface addresses, flags, and detailed packet statistics. |
| `ethtool -k <intf>` | Offload features (TSO, GSO, GRO, checksum offload, etc.). |
| `ethtool -l <intf>` | Number of RX/TX queues (channels). |
| `ethtool -c <intf>` | Interrupt coalescing settings (rx-usecs, tx-usecs, etc.). |
| `ethtool -g <intf>` | Ring buffer sizes (RX/TX ring depths). |
| `ethtool -S <intf>` | Driver-specific NIC statistics (detailed per-queue counters). |
| `ethtool --phy-statistics <intf>` | PHY-level statistics. |
| `tc -s qdisc show dev <intf>` | Traffic control queueing discipline stats (drops, backlog, etc.). |
| `tc -s class show dev <intf>` | Traffic control class stats. |
| `/sys/class/net/<intf>/statistics/*` | Kernel interface counters (rx_bytes, tx_packets, rx_dropped, etc.). |
| `/sys/class/net/<intf>/queues/**/*` | Per-queue settings (tx_timeout, rps_cpus, rps_flow_cnt, etc.). |

#### Network Namespace Data (for each namespace specified via `-n`)

All of the following are collected inside each namespace via `ip netns exec`:

- `/proc/net/dev`, `softnet_stat`, `netstat`, `snmp`, `snmp6`, `sockstat`
- `/proc/softirqs`
- `iptables -vnL`, `iptables -t nat -vnL`, `ip6tables -vnL`, `ip6tables -t nat -vnL`
- Per-interface: `ip addr show`, `ethtool -k/-l/-c/-g/-S/--phy-statistics`, `tc qdisc/class`

### B Section (every interval)

#### CPU

| Source | Purpose |
|--------|---------|
| `/proc/stat` | Aggregate and per-CPU time counters (user, nice, system, idle, iowait, irq, softirq, steal, guest). The primary source for CPU utilization calculation. Also contains total softirq counts, context switches, process creation rate. |
| `/proc/loadavg` | 1/5/15 minute load averages and running/total process counts. |
| `/proc/pressure/cpu` | PSI (Pressure Stall Information) for CPU — avg10, avg60, avg300 stall percentages. |
| `/proc/net/softnet_stat` | Also collected in B section for higher-frequency packet processing monitoring. |

#### Memory

| Source | Purpose |
|--------|---------|
| `/proc/meminfo` | Detailed memory breakdown: MemTotal, MemFree, MemAvailable, Buffers, Cached, SwapTotal, SwapFree, AnonPages, Mapped, Slab, PageTables, HugePages, etc. |
| `/proc/vmstat` | Virtual memory statistics: page faults, page-in/out, swap-in/out, OOM kills, compaction, etc. |
| `/proc/pressure/memory` | PSI for memory stalls. |

#### Disk I/O

| Source | Purpose |
|--------|---------|
| `/proc/diskstats` | Per-block-device I/O statistics: reads/writes completed, sectors transferred, time spent in I/O, in-flight requests. |
| `/proc/pressure/io` | PSI for I/O stalls. |

#### Cgroup Statistics

Automatically discovered via filesystem walk. Supports both cgroup v1 and v2.

**Cgroup v1:**

| Source | Purpose |
|--------|---------|
| `/sys/fs/cgroup/cpu/**/cpu.stat` | Per-cgroup CPU throttling: nr_periods, nr_throttled, throttled_time. |
| `/sys/fs/cgroup/memory/**/memory.stat` | Per-cgroup memory counters: cache, rss, mapped_file, pgfault, etc. |

**Cgroup v2:**

| Source | Purpose |
|--------|---------|
| `/sys/fs/cgroup/**/cpu.stat` | CPU usage and throttling per cgroup. |
| `/sys/fs/cgroup/**/memory.stat` | Memory usage breakdown per cgroup. |
| `/sys/fs/cgroup/**/memory.current` | Current memory usage of the cgroup in bytes. |
| `/sys/fs/cgroup/**/io.stat` | Per-device I/O statistics for the cgroup. |

#### QEMU Process Monitoring

For each running `qemu-system` process (discovered via `pgrep -x qemu-system`),
three levels of thread data are collected:

**1. QEMU process itself** (`/proc/<pid>/`):

| Source | Purpose |
|--------|---------|
| `/proc/<pid>/stat` | Process state, cumulative CPU time (utime + stime in clock ticks), priority, nice, num_threads, vsize, rss. Clock-tick precision (~10ms at HZ=100). |
| `/proc/<pid>/statm` | Memory usage in pages: total, resident, shared, text, data. |
| `/proc/<pid>/status` | Human-readable process status: UID, VmPeak, VmRSS, Threads, voluntary/involuntary context switches. |
| `/proc/<pid>/wchan` | Kernel function the process is currently blocked in (if sleeping). |
| `/proc/<pid>/sched` | Scheduler statistics: nr_switches, nr_voluntary_switches, exec_runtime, wait_sum, etc. |
| `/proc/<pid>/schedstat` | Raw scheduler accounting: time on CPU, time waiting on runqueue, number of timeslices. |

**2. In-process threads** (`/proc/<pid>/task/<tid>/`):

These are threads within the QEMU process itself, including vCPU threads
(typically named `CPU 0/KVM`, `CPU 1/KVM`, etc.), I/O threads, and the main
QEMU thread.

| Source | Purpose |
|--------|---------|
| `/proc/<pid>/task/<tid>/stat` | Per-thread CPU time (utime + stime). Allows precise per-vCPU CPU usage calculation. |
| `/proc/<pid>/task/<tid>/comm` | Thread name (e.g. `CPU 0/KVM`, `worker`, `SPICE main loo`). Identifies what each thread does. |

**3. Associated kernel threads** (separate PIDs):

The kernel creates helper threads for QEMU VMs that run as separate processes,
not inside QEMU's task directory. These are discovered by scanning `/proc/*/comm`
for thread names containing the QEMU PID.

Examples: `vhost-5756` (virtio host networking), `kvm-pit/5756` (KVM PIT timer
emulation), `kvm-nx-lpage-recovery-5756` (KVM large page recovery).

| Source | Purpose |
|--------|---------|
| `/proc/<kthread_pid>/stat` | CPU time for the kernel thread. `vhost-*` threads often consume significant CPU for network-heavy VMs. |
| `/proc/<kthread_pid>/comm` | Kernel thread name. |

This three-level collection provides complete CPU accounting for virtual machines:
the analyzer can compute per-vCPU usage, vhost overhead, and total VM cost by
summing the appropriate thread times from `/proc/*/stat` (fields 14+15: utime+stime),
which are in clock ticks — far more precise than the seconds-resolution TIME column
from `ps`.

## Crash Safety

Data is flushed to disk (`bufio.Flush()` + `fsync()`) every `-flush-interval`
collection intervals (default 6, i.e. every 60s at a 10s collection interval).
On crash, at most that many intervals of data are lost. Use `-flush-interval 1`
for maximum safety (flush every collection interval) at the cost of more disk
I/O. CBOR's self-describing framing means a truncated last record is detectable
and all prior records remain valid.

## Memory Usage

Each sample is CBOR-encoded directly to a 64KB buffered writer and not retained
in memory. The buffer is flushed each interval. Memory usage stays bounded at
~1-2MB regardless of how long the collector runs.
