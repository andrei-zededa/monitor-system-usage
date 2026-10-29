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
| `-include-env` | `filtered` | Environment to record in header: `filtered` (drop secret-looking keys), `all`, or `none` |
| `-ping` | (none) | IPv4 address to ping continuously; repeatable (or comma-separated). Counters are recorded in every B section as `ping <ip>` |
| `-ping-interval` | `100ms` | Time between ICMP echo requests sent to each `-ping` target (10 requests/s by default) |
| `-ping-timeout` | `500ms` | Time to wait for an echo reply before counting the request as lost |
| `-dump` | (none) | Dump a CBOR file to human-readable text and exit |
| `-version` | | Print version and exit |

### Examples

```sh
# Collect to a file, 10s interval:
msu-collect -o /persist/newlog/keepSentQueue/msu_data.msu.cbor

# Collect with network namespace monitoring:
msu-collect -n ns1,ns2 -o /persist/msu.cbor

# Also monitor reachability and latency of the gateway and a remote host
# (10 ICMP echo requests/s each, counters in every B section):
msu-collect -ping 192.168.1.1 -ping 8.8.8.8 -o /persist/msu.cbor

# Inspect a CBOR file:
msu-collect -dump /persist/msu.cbor | less

# Typical EVE-OS deployment:
eve enter debug
apk add --no-cache musl-utils iproute2 ethtool
msu-collect -interval 10 -o /persist/newlog/keepSentQueue/msu.cbor &
```

## Output Format

CBOR (RFC 8949) sequence — concatenated self-contained CBOR items. The
file starts with a single header record, followed by a mix of source
definitions and samples. Every record carries a `type` field so readers
can dispatch between the three record kinds.

The `msu` analyzer (`cmd/msu`) auto-detects CBOR vs legacy text format
and can process both transparently.

**Format version 2** — not backwards-compatible with v1. Timestamps are
now int64 unix-nanoseconds; `section`/`cmd`/`ns` are deduplicated via a
small integer source dictionary; the header carries additional
invocation metadata.

### Header Record

Written once at the start. Contains system and invocation metadata:

| Field | Description |
|-------|-------------|
| `v` | Format version (currently 2) |
| `type` | Always `"header"` |
| `ts` | Start time (int64 unix-nanoseconds UTC) |
| `msu_ver` | Collector version |
| `hz` | `CLK_TCK` — kernel clock ticks per second |
| `psz` | `PAGESIZE` in bytes |
| `cgroup_v` | Cgroup version (1 or 2) |
| `hostname` | `/proc/sys/kernel/hostname` |
| `kern_ostype` | `/proc/sys/kernel/ostype` (e.g. `Linux`) |
| `kern_release` | `/proc/sys/kernel/osrelease` (e.g. `6.18.19`) |
| `kern_version` | `/proc/sys/kernel/version` (e.g. `#1 SMP ...`) |
| `interval_ns` | Sampling interval in nanoseconds |
| `flush_every_n` | Flush every N collection intervals |
| `cmdline` | `os.Args` of the msu-collect invocation |
| `env` | Environment map (possibly filtered — see `env_mode`) |
| `env_mode` | `"filtered"`, `"all"`, or `"none"` |

**Env filtering**: by default (`-include-env=filtered`), keys matching
the case-insensitive regex
`(TOKEN|KEY|SECRET|PASSWORD|PASSWD|AUTH|CREDENTIAL|COOKIE)` are dropped
before the env map is written. Use `-include-env=none` to omit the env
entirely, or `-include-env=all` to capture every variable. Collected
files may still contain secrets from the output of monitored commands,
so treat them as sensitive regardless of env mode.

### Source Definition Record

Emitted inline the first time a given `(section, cmd, ns)` tuple is
used, just before the Sample that references it. Subsequent Samples
for the same tuple only carry the `src` ID.

| Field | Description |
|-------|-------------|
| `v` | Format version |
| `type` | Always `"src"` |
| `id` | uint16 source ID |
| `sec` | Section: `"init"`, `"A"`, or `"B"` |
| `cmd` | Canonical command identifier, e.g. `"cat /proc/stat"` |
| `ns` | Network namespace (omitted for root namespace) |

### Sample Record

One per collected file or command execution:

| Field | Description |
|-------|-------------|
| `v` | Format version |
| `type` | Always `"s"` |
| `ts` | Collection time (int64 unix-nanoseconds UTC) |
| `seq` | Monotonic interval counter |
| `src` | uint16 source ID — refers to a preceding SourceDef record |
| `out` | Raw output, newline-joined string |
| `err` | Error message if collection failed (omitted on success) |

## Collection Cadence

The collector runs in a loop with two section types, matching the original
shell script's behavior:

- **B sections** run every interval (default 10s) — lightweight, high-frequency data
- **A sections** run every 3rd interval (~30s) — heavier, less frequent data
- **Init** runs once before the loop — static system information
- **Ping monitors** (`-ping`) run continuously in background goroutines for the
  whole session, including init; their counters are snapshotted into every B section

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

#### ICMP Echo (Ping) Monitors

For each `-ping <IPv4>` target a background goroutine sends one ICMP echo
request every `-ping-interval` (default 100ms, i.e. 10 requests/s) and waits
up to `-ping-timeout` (default 500ms) for the reply. The monitors run for the
whole session, including the init phase, and their counters are snapshotted
into every B section as a sample whose `cmd` is `ping <ip>`. The output is
one `key value` pair per line, like `/proc/vmstat`:

```
target 10.0.0.1
mode raw
id 5662
interval_us 100000
timeout_us 500000
tx 61
tx_err 0
rx 61
rx_err 0
lost 0
late 0
dup 0
stray 0
pending 0
rtt_sum_us 5927
rtt_sumsq_us 721227
rtt_last_us 89
rtt_min_us 32
rtt_max_us 161
```

| Field | Meaning |
|-------|---------|
| `mode` | `raw` (SOCK_RAW ICMP socket, needs `CAP_NET_RAW`), `dgram` (unprivileged `IPPROTO_ICMP` datagram socket, needs the process gid inside `net.ipv4.ping_group_range`), or `none` while no socket could be opened — then the sample's `err` carries the reason and the monitor retries every 5s. |
| `id` | ICMP echo identifier used by this monitor (distinct per target). |
| `interval_us`, `timeout_us` | Effective `-ping-interval` / `-ping-timeout`. |
| `tx` | Echo requests handed to the kernel. |
| `tx_err` | Send attempts that failed (e.g. `network is unreachable`). The most recent error text since the previous sample is recorded in the sample's `err` field. |
| `rx` | Replies received within the timeout. |
| `rx_err` | Socket read errors. |
| `lost` | Requests with no reply within the timeout. |
| `late` | Replies that arrived after the timeout. Each one was already counted in `lost`. |
| `dup` | Additional replies for an already answered request. |
| `stray` | Replies carrying our identifier that match no outstanding request, or that came from a different source address. |
| `pending` | Requests still waiting for a reply at sample time (instantaneous, not cumulative). |
| `rtt_sum_us`, `rtt_sumsq_us` | Sum and sum of squares of in-time reply RTTs, in microseconds. Late replies are excluded. |
| `rtt_last_us` | RTT of the most recent in-time reply. |
| `rtt_min_us`, `rtt_max_us` | Smallest / largest RTT **since the previous sample**; `0` when no reply arrived in that interval. |

Everything except `pending`, `rtt_last_us`, `rtt_min_us` and `rtt_max_us` is
cumulative since the monitor started, so per-interval figures come from
differencing consecutive samples, exactly like `/proc` counters:

- requests sent in the interval: `Δtx`
- requests lost in the interval: `Δlost` (or `Δlost − Δlate` to ignore replies that merely came late)
- packet loss %: `Δlost / Δtx × 100`
- mean RTT: `Δrtt_sum_us / Δrx`
- RTT standard deviation: `sqrt(Δrtt_sumsq_us / Δrx − mean²)`

A request is only known to be lost once its timeout expires, so a request sent
at the very end of an interval is counted as lost in the next one.

The `msu` analyzer can track any of these fields directly:

```sh
msu -command "ping 10.0.0.1" -field-pattern '^lost ' -section-type B msu.cbor
```

Privileges: the monitor first opens a raw ICMP socket (`CAP_NET_RAW`, always
available as root in the EVE-OS debug container) and falls back to an
unprivileged ICMP datagram socket. In raw mode the kernel is asked via
`ICMP_FILTER` to deliver only echo replies, and replies are matched on
identifier, source address and sequence number. No external `ping` binary is
used.

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
