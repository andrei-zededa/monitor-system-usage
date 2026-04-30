# QEMU/KVM Network Overhead vs In-VM CPU Utilization

## The Discrepancy

For network-intensive VM workloads, the Zedcloud status endpoint `Cpu.Utilization` can be
2-4x higher than what `/proc/stat` inside the VM reports. This is not a
bug — the two numbers measure different things on purpose. See
`cpu-utilization-pipeline.md` for the API derivation.

### Worked example: test 3358 / 2026-03-23 (ubuntu_test_on_ENODE_TEST_AAAA_jt9)

A representative case where the gap was large enough to trace:

- VM has 6 vCPUs, host has 8 CPUs
- During 13:48-14:08 UTC (heavy network traffic):
  - Status endpoint (cpuacct.usage / vCPUs): **~38%**
  - In-VM /proc/stat busy (user+sys+sirq): **~11.2%**
  - Gap (QEMU/KVM overhead): **~27%** (72% of cpuacct.usage is overhead)
- In-VM softirq spike: cpu5 at 53% softirq, cpu3 at 6% softirq (network processing)
- Host softirq: 935.6s over 20min (bridge/packet processing, outside QEMU cgroup)
- Host system time: 1561.9s over 20min (largely KVM exit handling)

The qualitative pattern — cloud API number greatly exceeding in-VM
`/proc/stat`, host softirq+system clearly elevated — repeats every
time we put network load through a virtio-net VM. The numbers vary
with VM CPU count, packet rate, and queue topology; the structure
doesn't.

## Root Cause

`cpuacct.usage` (cgroups v1) measures ALL CPU time of the QEMU process, including:
1. **Guest vCPU execution** — what the VM sees in /proc/stat
2. **KVM exit handling** — host kernel time charged to QEMU threads (VMX transitions, interrupt injection, MMIO)
3. **QEMU userspace I/O** — virtio backend queue processing, device emulation
4. **QEMU management threads** — event loop, main thread

The guest `/proc/stat` only sees #1. Items #2-#4 are invisible to the guest but
charged to `cpuacct.usage` on the host.

### Where vhost CPU is accounted

This trips up almost every first-time investigation. The `vhost-<pid>`
threads that EVE-OS spawns for virtio-net data plane are **kernel
threads with their own PIDs**, scheduled outside the QEMU cgroup. So:

| Counter | Sees vhost CPU? |
|---------|-----------------|
| Guest `/proc/stat` | **No** — runs on the host. |
| `cpuacct.usage` for the QEMU cgroup | **No** — different cgroup. |
| Zedcloud `Cpu.Utilization` for the app instance | **No** — derives from `cpuacct.usage`. |
| Host `/proc/stat` (system + softirq) | **Yes** — softirq mainly, plus some system. |
| Host `ps auxwww` | **Yes** — visible as `[vhost-<pid>]`. |
| `/proc/<vhost-pid>/{stat,comm}` (the data msu-collect grabs via QEMU kernel-thread discovery) | **Yes** — this is the precise per-vhost CPU number. |

So a VM that looks 11% busy from inside, 38% busy via the cloud API,
and is also pushing a vhost thread to 60-90% on a host core, has its
true CPU cost spread across **three** counters that don't sum: the
guest's `/proc/stat`, the host's `cpuacct.usage`, and the host's
per-vhost-thread `/proc/<pid>/stat`. The Level 3 cross-reference is
exactly this reconciliation.

## EVE-OS Already Uses vhost-net

- Confirmed in `eve/pkg/pillar/hypervisor/kvm.go:369-377`: `vhost = "on"` for virtio-net-pci
- KVM capabilities: `UseVHost: true` (kvm.go:674)
- Multiple `[vhost-<pid>]` kernel threads visible in ps output (one per virtio-net queue)
- vhost-net moves data plane to kernel threads (outside QEMU cgroup)
- But KVM exits for virtio kicks/interrupt injection still go through QEMU threads

## Multiqueue virtio-net (the cheapest scaling fix)

By default a virtio-net device has **one** RX queue and **one** TX
queue, with **one** vhost thread per direction. All RX softirq work
inside the guest lands on a single vCPU, and all host-side data plane
work lands on a single vhost thread. This is the dominant cause of
the "VM caps at one core's worth of softirq" pattern.

Multiqueue virtio-net (`-netdev …,queues=N` on the QEMU side, plus
matching driver/ethtool settings inside the guest) gives N RX/TX queue
pairs, and the vhost subsystem spawns N vhost threads, so traffic
parallelizes across N host cores AND N guest vCPUs.

Diagnostic from inside the VM:

```bash
ethtool -l <intf>          # combined queues; > 1 means multiqueue
ethtool -L <intf> combined N    # raise to N (≤ vCPU count)
```

From the host: count `vhost-<qemu-pid>` threads in `ps auxwww` —
should equal the queue count.

**Why this is the first thing to try**: it's a guest-side ethtool
toggle (sometimes auto-detected, sometimes needs `combined N`), the
host already has the vhost threads ready, and it costs nothing in
config or platform support. SR-IOV / passthrough have much bigger
operational footprints; try multiqueue first.

## Alternatives to Reduce Overhead (from least to most disruptive)

| Approach | What it removes | Status in EVE-OS |
|----------|----------------|------------------|
| vhost-net | QEMU userspace virtio processing | Already enabled |
| SR-IOV | Entire virtual networking path + KVM exits | Supported by EVE-OS |
| PCI Passthrough | Everything (dedicated NIC) | Supported by EVE-OS |
| vhost-user + DPDK | Kernel transitions + interrupts | Not in EVE-OS |
| vDPA | SR-IOV perf + virtio portability | Needs vDPA NIC hardware |

- SR-IOV best for north-south traffic
- OVS-DPDK better for east-west (intra-node) traffic
- EVE-OS SR-IOV docs: https://wiki.lfedge.org/display/EVE/SR-IOV+Support

**IOMMU group caveat for SR-IOV / passthrough**: a Virtual Function
can only be passed to a VM as a unit with everything else in its
IOMMU group. On well-designed server boards each VF is in its own
group; on consumer hardware whole PCIe slots can share one group,
making passthrough impossible without ACS-override hacks. Check
before promising the topology will work:

```bash
find /sys/kernel/iommu_groups/ -type l | sort -V
# or, scoped to a device:
readlink /sys/bus/pci/devices/<bdf>/iommu_group
```

## Key Files
- Host msu.out: `tests/<ticket>/<date>/ENODE_TEST_AAAA_jt9_monitor_system_usage.out`
- VM msu.out: `tests/<ticket>/<date>/ubuntu_test_on_ENODE_TEST_AAAA_jt9_monitor_system_usage.out`
- Status CPU util: `tests/<ticket>/<date>/<name>.status.cpu_util.{10s,60s}`
- CPU total timeseries: `tests/<ticket>/<date>/<name>.timeSeries.CPU_TOTAL.60s`
