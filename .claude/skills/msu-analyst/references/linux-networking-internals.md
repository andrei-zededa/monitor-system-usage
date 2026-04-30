# Linux Networking Internals Reference

Domain knowledge for diagnosing network performance bottlenecks in Linux.

## Packet Reception Path

```
NIC hardware
  -> DMA to ring buffer (pre-allocated sk_buffs)
  -> Hardware interrupt (IRQ) raised on a specific CPU
  -> IRQ handler: acknowledge IRQ, schedule NAPI poll (raise NET_RX softirq)
  -> softirq context (NET_RX_SOFTIRQ):
      -> NAPI poll function: pull packets from ring buffer
      -> GRO (Generic Receive Offload): coalesce related packets
      -> Pass up to network stack (netif_receive_skb)
      -> Protocol handlers (IP, TCP/UDP)
      -> Socket receive buffer
      -> Application read()
```

## Packet Transmission Path

```
Application write()/sendmsg()
  -> Socket send buffer
  -> Protocol layer (TCP/UDP, IP routing)
  -> Traffic control (qdisc)
  -> Device driver xmit function
  -> DMA to NIC TX ring buffer
  -> Hardware transmit
  -> TX completion interrupt (NET_TX softirq for cleanup)
```

## SoftIRQ Processing

SoftIRQs are deferred work scheduled by hardware interrupt handlers. Network
packet processing happens in softirq context, not in the hardware IRQ handler
itself.

### Key Parameters

| Parameter | Typical default | Description |
|-----------|-----------------|-------------|
| `net.core.netdev_budget` | 300 | Max packets processed per softirq invocation |
| `net.core.netdev_budget_usecs` | 2000 | Max microseconds per softirq invocation |

The NAPI poll loop processes up to `netdev_budget` packets or runs for up to
`netdev_budget_usecs` microseconds, whichever comes first. If the budget is
exhausted and there are still packets pending, the NAPI poll is rescheduled.

**Verify before tuning.** "Typical default" is mainline-kernel default. Stripped
kernels (EVE-OS, embedded, hardened distros) sometimes don't expose a knob at
all under `/proc/sys/net/core/`. Always run
`sysctl -a 2>/dev/null | grep -E 'netdev_(budget|max_backlog)'` against the
target system before recommending a value — if the knob is absent, retuning is
not the answer.

### ksoftirqd

When softirqs are raised too frequently or take too long, the kernel defers
them to `ksoftirqd/N` kernel threads (one per CPU). This prevents softirq
processing from starving user-space processes but can introduce latency.

## /proc/net/softnet_stat

One line per CPU core, all values in hex. **Column count grows over kernel
versions** — older 4.x kernels emit 11 columns, 5.10+ added flow-limit and
XDP-related counters, and 6.18 emits 15. Read by position only for the
stable columns 1-3 below; for everything else, cross-reference the live
kernel by counting fields and consulting
`Documentation/networking/snmp_counter.rst` and `net/core/net-procfs.c` in
the matching kernel source tree (the layout is hardcoded in `softnet_seq_show`).

| Column (1-indexed) | Field | Stability | Description |
|--------|-------|-----------|-------------|
| 1 | `total` | stable | Total packets processed in NAPI poll |
| 2 | `dropped` | stable | Packets dropped because per-CPU backlog was full (`netdev_max_backlog`) |
| 3 | `time_squeeze` | stable | Times the softirq exited because budget/time ran out |
| 4-8 | (varies) | unstable | Includes `cpu_collision` and per-version internals; often zeros |
| varies | `received_rps` | post-2.6.35 | Packets steered to this CPU via RPS |
| varies | `flow_limit_count` | post-3.11, `CONFIG_NET_FLOW_LIMIT` | Packets dropped by per-flow limiter (zero if disabled, not necessarily a problem) |
| (5.10+) | `softnet_backlog_len`, then XDP counters | recent | Most recent additions; check kernel version. |

### Key Indicators

- **Column 2 increasing (dropped)**: Backlog queue is full. Increase
  `net.core.netdev_max_backlog` (typical default 1000) — but only after
  confirming the knob exists on the target.
- **Column 3 increasing (time_squeeze)**: The softirq ran out of its time/packet
  budget before finishing all pending work. Increase `net.core.netdev_budget`
  and/or `net.core.netdev_budget_usecs`. If `ksoftirqd/N` is also chewing CPU
  in `ps auxwww`, the kernel is already pushing softirq work into thread
  context — confirms the diagnosis.

Example (kernel 6.18, 15 columns; first row is CPU 0):
```
00f4e773 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000
000cd0b8 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000000 00000001 00000000 00000000
```

## /proc/interrupts

Shows per-CPU interrupt counts. Relevant columns for NIC analysis:

```
           CPU0       CPU1       CPU2       ...
 42:          0          0          0       PCI-MSI  eth0-TxRx-0
 43:          0          0      12345       PCI-MSI  eth0-TxRx-1
```

Look for NIC IRQ lines (containing interface name or `TxRx`). The CPU column
with the highest count is handling that queue's interrupts.

### IRQ Affinity

Each IRQ has a CPU affinity mask: `/proc/irq/<IRQ>/smp_affinity_list`

## RSS (Receive Side Scaling)

Hardware-level packet distribution across multiple RX queues, each with its own
IRQ pinned to a specific CPU core.

### Configuration

```bash
# View current channel count
ethtool -l <dev>

# Set channel count
ethtool -L <dev> combined N

# View IRQ affinity for NIC queues
for irq in $(grep <dev> /proc/interrupts | awk '{print $1}' | tr -d ':'); do
    echo "IRQ $irq -> $(cat /proc/irq/$irq/smp_affinity_list)"
done
```

### How RSS Works

The NIC hashes packet headers (src/dst IP, src/dst port, protocol) to select
an RX queue. Each queue has a dedicated IRQ, typically pinned to a specific CPU.
This distributes packet processing across cores.

## IRQ Coalescing / Moderation

Coalescing lets the NIC delay raising an IRQ until either N packets have
arrived (`rx-frames`) or `rx-usecs` microseconds have elapsed since the
first one. Trades latency for fewer IRQs and better cache locality.

```bash
ethtool -c <dev>          # show current settings
ethtool -C <dev> rx-usecs 50 rx-frames 32 adaptive-rx off
```

`adaptive-rx on` (the common default) lets the driver auto-tune
coalescing under load. That can **mask** a softirq bottleneck: as
load rises, the driver silently raises `rx-usecs`, hiding the
overflow that absent moderation would have made obvious.
When measuring, force `adaptive-rx off` and pick a fixed value to
keep behavior reproducible. The collected `ethtool -c <intf>` data
in A sections records the live values.

## RPS (Receive Packet Steering)

Software-level packet distribution, used when hardware RSS is unavailable or
insufficient. Configured per RX queue:

```bash
# View RPS CPU mask for queue 0
cat /sys/class/net/<dev>/queues/rx-0/rps_cpus

# Set RPS CPU mask (hex bitmask)
echo "ff" > /sys/class/net/<dev>/queues/rx-0/rps_cpus
```

## RFS (Receive Flow Steering)

Cache-locality complement to RPS. RPS hashes flows to CPUs; RFS instead
remembers which CPU last `recv()`'d a given flow and steers subsequent
packets there, so the consuming socket and the softirq processing share
warm L1/L2 caches.

```bash
# Global flow table (per-socket consumer hint cache)
sysctl net.core.rps_sock_flow_entries        # typical: 32768
sysctl -w net.core.rps_sock_flow_entries=32768

# Per-RX-queue table (one entry per active flow)
echo 4096 > /sys/class/net/<dev>/queues/rx-0/rps_flow_cnt
```

Sum of per-queue `rps_flow_cnt` should equal `rps_sock_flow_entries`.
RFS only helps when packet-rate per flow is high enough that cache
warmth matters; on low-rate, many-flow workloads it adds bookkeeping
overhead with no win.

## XPS (Transmit Packet Steering)

Maps TX queues to CPUs for efficient transmit:
```bash
cat /sys/class/net/<dev>/queues/tx-0/xps_cpus
```

## Ring Buffer Sizes

```bash
# View current and maximum ring buffer sizes
ethtool -g <dev>

# Set ring buffer sizes
ethtool -G <dev> rx N tx N
```

Larger ring buffers absorb bursts but increase latency.

## NIC Offloads

`ethtool -k <dev>` lists segmentation, reception, and checksum offload
features. The msu-collect A section captures this per interface. Common
ones:

| Feature | What it does |
|---------|--------------|
| `tx-checksumming` | Hardware computes L4 checksum on TX. |
| `tso` (TCP Segmentation Offload) | Driver hands the NIC large TCP "super-segments"; NIC slices into MTU-sized frames. |
| `gso` (Generic Segmentation Offload) | Software-side counterpart to TSO; segmentation deferred until just before driver xmit. |
| `gro` (Generic Receive Offload) | Coalesces incoming TCP segments into super-segments before stack handoff. |
| `lro` (Large Receive Offload) | Hardware version of GRO; lossy for forwarding because it reorders. |

**Rule of thumb when measuring:**
- For **throughput/Mbps**: leave offloads on. Linux line-rate at 10/25/40/100G assumes them.
- For **packet-rate (kpps) / per-packet cost**: turn them off
  (`ethtool -K <dev> tso off gso off gro off`). Otherwise the kernel
  sees one "packet" per super-segment and your packet counters lie.
- For **forwarding / bridge / NAT** workloads: GRO can help RX-side
  CPU but LRO must be off (it merges packets that need to be forwarded
  separately).

## BQL (Byte Queue Limits)

TX-side analog to softnet_stat: prevents excessive bufferbloat in the
driver's TX ring by capping bytes-in-flight. Per TX queue:

```bash
ls /sys/class/net/<dev>/queues/tx-0/byte_queue_limits/
# inflight     - bytes currently queued in the driver
# limit        - active cap, auto-tuned by the kernel
# limit_max    - hard ceiling
# limit_min    - hard floor
```

If `inflight` regularly hits `limit` and `tc -s qdisc` shows backlog
growing on the qdisc above it, the driver TX ring is the bottleneck —
either the link is saturated (legitimate) or BQL has clamped too low
for the workload.

## Per-Interface Statistics

Available at `/sys/class/net/<dev>/statistics/`:

| Counter | Description |
|---------|-------------|
| `rx_packets` | Total received packets |
| `tx_packets` | Total transmitted packets |
| `rx_bytes` | Total received bytes |
| `tx_bytes` | Total transmitted bytes |
| `rx_dropped` | Packets dropped by the kernel (queue full, memory) |
| `tx_dropped` | Packets dropped on transmit |
| `rx_errors` | Total receive errors |
| `tx_errors` | Total transmit errors |
| `rx_missed_errors` | Packets missed by NIC (ring buffer overflow) |
| `rx_fifo_errors` | FIFO overrun errors |
| `rx_over_errors` | Receiver ring buffer overflow |

### Driver-Level Statistics

`ethtool -S <dev>` provides driver-specific counters including per-queue
packet/byte counts, hardware drop counters, and CRC errors.

## PSI (Pressure Stall Information)

`/proc/pressure/{cpu,memory,io}` reports the **fraction of wall-clock
time** the system was stalled waiting on each resource. msu-collect
captures all three in B sections.

```
$ cat /proc/pressure/cpu
some avg10=4.32 avg60=2.10 avg300=1.05 total=12345678
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
```

- `some` — at least one task was stalled on this resource.
- `full` — *every* runnable task was stalled (memory/io only;
  not meaningful for CPU since the stalled task itself counts).
- `avg10/60/300` — percent of the trailing 10/60/300 seconds spent
  in the stall state.

Compared to `/proc/loadavg`: load average can't tell you whether the
CPU itself is the constraint (a load of 8 on 8 CPUs vs 8 on 32 CPUs
look identical). PSI tells you directly: "the CPU was the bottleneck
for X% of the last minute". `cpu.some.avg10` rising past ~10-20%
under sustained workload is a confident signal of CPU saturation.

For VMs: `cpu.full` doesn't exist (per kernel), but `memory.full` and
`io.full` are diagnostic for "the entire VM was stalled" — far more
useful than guessing from `vmstat` swap activity.

## Common Bottleneck Patterns

### 1. Single-Core CPU Saturation (softirq)

**Signature**: One core shows ~100% softirq in `/proc/stat`. Other cores idle.
Throughput caps well below NIC line rate.

**Cause**: All NIC interrupts routed to one CPU. NAPI poll on that CPU cannot
keep up with packet arrival rate.

**Fix**: Enable RSS (`ethtool -L <dev> combined N`), verify IRQ affinity
distribution, or enable RPS.

### 2. Single-Core CPU Saturation (user-space)

**Signature**: One core shows high user% (iperf process). Throughput caps at
that core's capacity.

**Cause**: iperf client or server bound to one core, single-threaded.

**Fix**: Use multiple iperf flows pinned to different cores.

### 3. Ring Buffer Overflow

**Signature**: `rx_missed_errors` or `rx_over_errors` incrementing in
`/sys/class/net/<dev>/statistics/`. May also show in `ethtool -S` as
hardware-specific drop counters.

**Cause**: NIC ring buffer too small for burst traffic.

**Fix**: Increase ring buffer size: `ethtool -G <dev> rx 4096`.

### 4. Kernel Backlog Overflow

**Signature**: Column 2 of `/proc/net/softnet_stat` increasing (non-zero
second hex value).

**Cause**: `net.core.netdev_max_backlog` too small. Packets arriving faster
than the CPU can process them from the per-CPU backlog queue.

**Fix**: `sysctl -w net.core.netdev_max_backlog=10000`.

### 5. SoftIRQ Budget Exhaustion

**Signature**: Column 3 of `/proc/net/softnet_stat` increasing (time_squeeze).
Core may show high softirq% but not quite 100%.

**Cause**: `netdev_budget` or `netdev_budget_usecs` too low. The softirq
handler exits before processing all pending packets, causing rescheduling
overhead.

**Fix**:
```bash
sysctl -w net.core.netdev_budget=600
sysctl -w net.core.netdev_budget_usecs=4000
```

### 6. UDP Socket Buffer Overflow

**Signature**: iperf reports loss (Lost/Total in Server Report) but no
corresponding kernel-level drops (interface counters, softnet_stat all clean).
For confirmation, watch `Udp.RcvbufErrors` in `/proc/net/snmp` — it
increments exactly when this happens.

**Cause**: The receiving application (iperf server) cannot read from the
UDP socket fast enough. Packets are delivered to the kernel socket buffer but
dropped when it fills.

**Fix**: Increase socket buffer:
```bash
sysctl -w net.core.rmem_max=26214400
sysctl -w net.core.rmem_default=26214400
```
Or iperf server flag: `iperf -s -u -w 16M`.

**TCP vs UDP buffer knobs** — easy to confuse:

| Knob | What it gates |
|------|---------------|
| `net.core.rmem_max` / `wmem_max` | Hard ceiling on what `setsockopt(SO_RCVBUF/SO_SNDBUF)` can request. Mostly relevant for UDP and apps that explicitly set `SO_RCVBUF`. |
| `net.ipv4.tcp_rmem` / `tcp_wmem` | 3-tuple `min default max`. TCP **autotunes** per socket within this range; you almost never need to touch `rmem_max` for TCP. |
| `net.core.rmem_default` / `wmem_default` | Initial buffer size for sockets that don't set `SO_RCVBUF` and aren't TCP autotuned (mainly UDP). |

So for UDP throughput tests: tune `rmem_max`/`rmem_default` (and pass
`-w` to iperf). For TCP throughput problems: look at `tcp_rmem[2]`
(the autotune ceiling) and `tcp_mem`. Setting `rmem_max` for a TCP
problem is almost always cargo-cult.

### 7. IRQ Distribution Imbalance

**Signature**: `/proc/interrupts` shows NIC IRQ counts concentrated on one or
few CPUs. Uneven softirq load across cores.

**Cause**: RSS not configured, or `irqbalance` daemon not distributing
effectively, or affinity manually set incorrectly.

**Fix**: Stop `irqbalance`, manually set per-queue IRQ affinity to distribute
across cores.

### 8. ksoftirqd Burning CPU

**Signature**: `ksoftirqd/N` shows nontrivial CPU in `ps auxwww`
(captured by msu-collect's A section). Often co-occurs with bottleneck
#5 (time_squeeze).

**Cause**: When NAPI repeatedly exhausts its budget, the kernel
gives up on running softirqs back-to-back from interrupt context and
schedules `ksoftirqd/N` to drain them in a thread context — at the
mercy of the scheduler. This shifts cost from "stolen from
userspace" to "scheduled work that competes with userspace", which
behaves differently and is usually worse for latency.

**Fix**: this is a confirmation of an underlying softirq overload, not
the disease itself. Treat #1 (single-core softirq) or #5 (budget
exhaustion). If CPU isolation is in play, make sure the workload
core's `ksoftirqd/N` isn't pinned away from where the softirq is
landing.

## Tuning Checklist

1. **Check RSS/queue count**: `ethtool -l <dev>` - match queue count to available cores
2. **Check IRQ affinity**: Verify each NIC queue IRQ is pinned to a separate core
3. **Check IRQ coalescing**: `ethtool -c <dev>` - force `adaptive-rx off` when measuring; otherwise the driver hides the bottleneck
4. **Check offloads**: `ethtool -k <dev>` - turn TSO/GSO/GRO off for kpps measurements; leave on for throughput
5. **Check ring buffers**: `ethtool -g <dev>` - increase if seeing missed/overflow errors
6. **Check softnet_stat**: Monitor columns 2 and 3 for drops and squeezes
7. **Check ksoftirqd**: If `ksoftirqd/N` is hot in `ps`, you're already over budget — go fix #5 not the symptom
8. **Check socket buffers**: `sysctl net.core.rmem_max` for UDP; for TCP look at `tcp_rmem[2]`
9. **Check netdev_budget**: Increase if time_squeeze is incrementing (and the knob exists on the target)
10. **Check CPU affinity**: Ensure iperf processes are pinned to cores not used for IRQ processing
11. **Check PSI**: `/proc/pressure/cpu`'s `some.avg10` confirms whether CPU is actually the bound

## When Classic Tuning Isn't Enough: XDP / AF_XDP

For workloads that need millions of pps per core or sub-microsecond
forwarding latency, the classic `softnet → IP → socket` path is
inherently expensive. **XDP** (eXpress Data Path) attaches an eBPF
program at the driver's RX hook, runs before `sk_buff` allocation,
and can `DROP`/`PASS`/`TX`/`REDIRECT` packets at near-NIC line rate.
**AF_XDP** is a userspace socket family that lets a process bypass
the kernel stack entirely while still using kernel drivers. Both
require driver support (`ethtool -i <dev>` lists features).

These are alternative architectures, not knobs you turn. Reach for
them only after ruling out everything in the checklist above; the
ergonomics cost is real.
