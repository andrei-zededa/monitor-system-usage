# Monitor System Usage

**monitor-system-usage** (**msu**) is a system performance monitoring and
analysis toolkit written in Go, built for diagnosing performance issues in the
CPU / network throughput areas with an emphasis on virtualization scenarios,
QEMU/KVM VMs running on EVE-OS.

The toolkit tries to help with a specific operational problem: **understanding
what is consuming CPU and network resources on an EVE-OS host**, especially
when that host runs QEMU/KVM virtual machines doing high-throughput networking.

Key questions it tries to answer:

- Is CPU utilization driven by user processes, kernel networking (softirq), or
  VM overhead (KVM exits)?
- Are any individual CPU cores saturated?
- Are packets being dropped, and where in the stack (NIC ring buffer, kernel
  backlog, socket buffer)?
- Why does the cloud management API (Zedcloud) report different CPU numbers
  than what `/proc/stat` shows inside the VM?
- At what offered network load (kpps) does the system hit a bottleneck, and
  what is the root cause?

The system is split into a **collector** and an **analyzer**, which run
independently:

```
    EVE-OS Device                               Offline analysis 
 ┌──────────────────┐                  ┌──────────────────────────────┐
 │  msu-collect     │   .msu.cbor      │                              │
 │  (could run for  │─────────────────>│                              │
 │   a long time    │ copy off device  │  msu command reads one or    │ 
 │   inside debug   │                  │  more .msu.cbor files and    │
 │   container)     │                  │  produces reports or gives   │
 │                  │                  │  access to command/output    │
 │   Workload VM    │                  │  statistics.                 │
 │  ┌─────────────┐ │                  │                              │
 │  │msu-collect  │ │   .msu.cbor      │  Claude skill uses msu to    │
 │  │can also run │───────────────────>│  highlight CPU staturation,  │
 │  │inside of VM │ │  copy off VM     │  packet drops, etc.          │
 │  └─────────────┘ │                  │                              │
 │                  │                  │                              │
 └──────────────────┘                  └──────────────────────────────┘
```

## Installing

`msu-collect` is published as a static `linux/amd64` binary in each GitHub
release. The `scripts/install.sh` helper resolves the latest release tag,
downloads the binary, verifies its SHA256 against the release's `SHA256SUMS`
file, and installs it to `/usr/local/bin` (override with `PREFIX=...`; pin a
specific release with `VERSION=vX.Y.Z`).

Install the script bundled with the latest release (preferred — pinned to a
tagged, checksum-verified asset):

```sh
curl -sSLf https://github.com/andrei-zededa/monitor-system-usage/releases/latest/download/install.sh | sh
```

Install the script straight from `main` (useful when iterating on the installer
itself, before a release exists):

```sh
curl -sSLf https://raw.githubusercontent.com/andrei-zededa/monitor-system-usage/main/scripts/install.sh | sh
```

## msu-collect

A lightweight, long-running binary that samples system state at regular
intervals and writes structured CBOR records to a file.

**Key characteristics:**
- **Memory-bounded**: ~1-2 MB constant usage regardless of how long it runs
  (hours, days). Samples are streamed to disk, never accumulated in memory.
- **Crash-safe**: CBOR framing means a truncated final record is detectable
  and all prior records remain valid. Configurable flush/fsync intervals.
- **Two collection cadences**: Lightweight "B sections" every 10 seconds
  (CPU, memory, disk, QEMU stats); comprehensive "A sections" every 30
  seconds (full process listing, per-interface ethtool stats, firewall
  rules, routing tables, cgroup metrics).
- **Dynamic discovery**: Automatically finds network interfaces, QEMU
  processes and their threads, cgroup hierarchies, and network namespaces.
- **Replaces a shell script**: The original `monitor_system_usage.sh` is
  included for legacy compatibility; `msu-collect` produces equivalent data
  in a structured binary format.

## msu

A multi-mode CLI tool that reads collected data and produces:

1. **Section listings** — time ranges and durations of A/B collection
   intervals
2. **Command tracking** — follow a specific counter (e.g., `rx_dropped`)
   over time with per-sample rates
3. **Change detection** — find all counters that changed during a time
   window
4. **CPU comparison charts** — interactive HTML with Chart.js showing
   per-core CPU breakdown, softirq rates, network stats, and optional
   Zedcloud API overlay
5. **Unified HTML reports** — full diagnostic reports combining all of the
   above, optionally correlated with iperf throughput test results

The analyzer auto-detects file format (CBOR or legacy text) and supports
flexible time filtering.

## What gets collected

The collector captures a snapshot of system state, organized into two tiers:

### B-Section (every 10s — lightweight, high-frequency)
- CPU time per core (`/proc/stat`)
- Memory breakdown (`/proc/meminfo`, `/proc/vmstat`)
- Load averages, disk I/O, pressure stall info (PSI)
- Cgroup CPU/memory/IO stats (v1 and v2)
- Per-QEMU process: `/proc/<pid>/stat`, status, scheduler stats
- Per-QEMU thread: `/proc/<pid>/task/<tid>/stat`
- Associated kernel threads (vhost-*, kvm-*)

### A-Section (every 30s — comprehensive, heavier)
- Full process listing (`ps auxwww`)
- Interrupt and softirq distribution
- Network stack counters (6 `/proc/net/*` files)
- Firewall rules with packet/byte counters
- Bridge forwarding database, VLAN config, full routing table
- Per-interface: `ip addr`, ethtool features/queues/ring/stats, traffic control
- Sysfs per-interface and per-queue counters
- Conntrack state
- All of the above repeated per monitored network namespace
