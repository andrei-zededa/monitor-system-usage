# CPU Utilization Pipeline: EVE-OS → Zedcloud → API

## 1. Collection in EVE-OS (domainmgr)

### Device-level (host) CPU
- File: `eve/pkg/pillar/cmd/domainmgr/metric.go:178-215`
- Uses gopsutil `cpu.Times(false)` which reads `/proc/stat`
- Computes: busy = User + System + Nice + Irq + Softirq
- Divides by number of CPUs (per-CPU average)
- Converts to nanoseconds: `CPUTotalNs = busy * 1e9`
- Published with nil UUID to identify as host metric

### App instance CPU (VM/container)
- File: `eve/pkg/pillar/cmd/domainmgr/metric.go:94-148`
- containerd: reads cgroups v1 `cpuacct.usage` file
  - File: `eve/pkg/pillar/vendor/github.com/containerd/cgroups/cpuacct.go:52-69`
  - Reads `/sys/fs/cgroup/cpuacct/<cgroup>/cpuacct.usage` (cumulative nanoseconds)
  - This is total CPU time of ALL tasks in cgroup (all QEMU threads for a VM)
- xen: reads from xenstat
- Divides by vCPU count: `dm.CPUTotalNs /= uint64(status.VCpus)` (line 125)
- All values are cumulative monotonic counters, not percentages

### Collection interval
- domainmgr collects 4x more frequently than zedagent metric interval
- Default metric interval: 60s (configurable via `timer.metric.interval`, min 5s, max 3600s)

## 2. Transmission to Zedcloud (zedagent)

- File: `eve/pkg/pillar/cmd/zedagent/handlemetrics.go:575-589`
- Converts to seconds for protobuf: `CpuMetric.Total = CPUTotalNs / 1e9`
- Also sends nanoseconds in `CpuMetric.TotalNs`
- Protobuf message: `ZMetricMsg` containing `AppCpuMetric`
- API path: `POST /v2/device/{deviceUUID}/metrics`
- Reporting interval: ~60s with 30%-100% jitter

## 3. Storage in Zedcloud

The server-side persistence layer is **not** in the EVE-OS open-source
repo and the items below are inferred from API behavior rather than
read directly from server code:

- Each `CpuMetric.Total` (seconds, cumulative) lands in a per-app or
  per-device time series keyed by `(deviceUUID, appUUID, timestamp)`.
- Storage is the **raw cumulative counter**, not a precomputed rate.
  Rates are derived at query time, which is why the same underlying
  data can yield different "utilization" numbers depending on which
  API is queried.
- The host metric (nil app UUID) and per-app metrics live in the same
  table and are distinguished by the UUID alone.

## 4. Zedcloud API Response Calculation

Four endpoint families surface the data, all derived from the same
cumulative-counter time series. Numbers below are inferred from
observed responses against EVE-OS deployments (test 3358/2026-03-23
and the one-hour host-window behavior described in `SKILL.md:147-149`):

- **`status.cpu_util.10s` / `status.cpu_util.60s`**
  (tab-separated, `timestamp\tvalue`).
  - **App instances**: rate over the named window —
    `(last - first) * 100 / window_seconds / vCPUs`.
    Because the underlying counter is `cpuacct.usage` (all QEMU
    threads), the resulting percentage **includes** KVM exit
    handling and QEMU userspace I/O on top of guest vCPU
    execution. See `qemu-network-overhead.md`.
  - **Host (nil UUID)**: averaged over a longer reporting window —
    typically ~1 hour — which is why short host spikes are smoothed
    out and `/proc/stat` snapshots can disagree dramatically with
    the API's host CPU number.
- **`timeSeries.CPU_USAGE.*` / `timeSeries.CPU_TOTAL.*`** (JSON with
  matching `timestamps` and `values` arrays). These are the raw
  cumulative-counter samples indexed by RFC 3339 timestamp; the
  consumer is expected to compute deltas itself. The `*` suffix in
  the filename is the bucket size (`60s` is what we've observed).

**Practical rule for the analyst**: when the API number disagrees with
`/proc/stat` inside the VM, do not assume a bug. Reach for
`qemu-network-overhead.md` first.

## Summary Table

| Stage | Source | Value format | Notes |
|-------|--------|--------------|-------|
| EVE collection (host) | `/proc/stat` via gopsutil | Nanoseconds, cumulative | `User+System+Nice+Irq+Softirq`, divided by CPU count. |
| EVE collection (VM, kvm) | xenstat | Nanoseconds, cumulative | xen hypervisor path. |
| EVE collection (VM, container) | cgroups v1 `cpuacct.usage` | Nanoseconds, cumulative | All QEMU process CPU time, divided by `VCpus`. **Includes** KVM exit handling, QEMU userspace, management threads. |
| EVE transmission | `CpuMetric.Total` (and `TotalNs`) | Seconds in protobuf | Reporting interval ~60s with 30%-100% jitter. |
| Zedcloud storage *(inferred)* | per-(device,app,ts) row | Cumulative counter, not a rate | Raw values; rates are derived at query time. |
| API: `status.cpu_util.{10s,60s}` *(inferred)* | server-side delta | Percent | App: `(last-first)*100/window/vCPUs`. Host: averaged over ~1h reporting window. |
| API: `timeSeries.CPU_{USAGE,TOTAL}.*` *(inferred)* | server-side passthrough | Cumulative counters, RFC 3339 timestamps | Consumer computes deltas. |
