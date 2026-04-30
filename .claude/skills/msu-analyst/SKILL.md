---
name: msu-analyst
description: >-
  Analyze system monitoring data from msu-collect (msu.out or .msu.cbor files)
  to diagnose CPU utilization, per-core saturation, network performance, softirq
  bottlenecks, packet drops, QEMU/KVM overhead, and Zedcloud API CPU
  discrepancies. Works progressively: single system MSU analysis, Zedcloud API
  comparison, cross-VM analysis, and iperf throughput correlation. Use this skill
  whenever the user mentions msu.out, msu.cbor, CPU utilization analysis, softirq
  rates, softnet_stat, packet drops, network interface counters, iperf throughput
  tests, kpps, QEMU overhead, EVE-OS performance, system monitoring data, or wants
  to troubleshoot CPU or network performance on a Linux system or EVE-OS device.
  Also triggers on: msu-collect, cpu_compare, core saturation, vhost threads,
  cpuacct.usage discrepancy, traffic generator, TGS, bridging performance.
---

# MSU Analyst

## Overview

This skill analyzes system monitoring data collected by `msu-collect` (or the
legacy `scripts/monitor_system_usage.sh`) to diagnose CPU and network
performance issues. It works on simple Linux systems as well as EVE-OS hosts
running QEMU/KVM VMs.

The analysis is **progressive** — it adapts to the data available:

| Level | Available Data | What You Get |
|-------|---------------|--------------|
| **1** | MSU output file only | CPU breakdown, per-core saturation, interface traffic/drops, softirq rates, QEMU thread CPU |
| **2** | + Zedcloud API data | Cloud API vs /proc/stat comparison, cpuacct.usage overhead analysis |
| **3** | + VM-internal MSU data | Cross-reference host vs guest CPU, network overhead attribution |
| **4** | + iperf test results | Rate-step analysis: throughput vs CPU vs drops, root cause determination |

## Prerequisites

### Tools

The analyzer is the `msu` binary built from this repo. The skill's command
examples use a `$MSU` shell variable so they work whether you built locally
or installed a release:

```bash
# Local build (from repo root)
go build -o msu ./cmd/msu/
export MSU=./msu

# OR: release binary installed by scripts/install.sh
export MSU=/usr/local/bin/msu
```

Python 3 is needed only for Level 4 (iperf parsing).

For the collector side (`msu-collect` CLI options, source inventory, CBOR
schema), see `cmd/msu-collect/README.md` — that's the authoritative
reference. `references/msu-tool-reference.md` summarizes both binaries from
the analyst's perspective.

### Data Files

MSU output files are produced by `msu-collect` (CBOR format, `.msu.cbor`) or the
legacy shell script (`_monitor_system_usage.out`). New captures should be
CBOR; legacy `.out` files still work because `msu` auto-detects format.
Place all related files for a test in a single directory.

## Step 0: Discover Data and Determine Level

Scan the test directory to determine what data is available:

```bash
ls <test-dir>/
```

| File pattern | Indicates |
|---|---|
| `*.msu.cbor` (current) or `*_monitor_system_usage.out` (legacy text) | MSU data (Level 1+) |
| `*.status.cpu_util.10s` or `*.status.cpu_util.60s` | Zedcloud API status (Level 2+) |
| `*.timeSeries.CPU_USAGE.*` or `*.timeSeries.CPU_TOTAL.*` | Zedcloud API timeSeries (Level 2+) |
| Multiple MSU files with host+VM naming | Cross-VM data (Level 3) |
| `run.sh` + `client_iperf_*.out` or pre-built `iperf.json` | iperf test data (Level 4) |

`.msu.cbor` is what `msu-collect` writes today; `*_monitor_system_usage.out`
is what the older shell collector produced and may still appear in archived
test bundles. `msu` reads both transparently.

## Quick `msu-collect` Cookbook

The user typically arrives at this skill with files already captured. If
they need to capture more, three common invocations cover most cases (full
flag reference: `cmd/msu-collect/README.md`):

```bash
# Long-running device capture — survives reboots, crash-safe.
msu-collect -interval 10 -o /persist/newlog/keepSentQueue/msu.cbor

# Traffic-generator setup with workload in network namespaces.
# Adds per-namespace /proc/net/* and iptables snapshots in A sections.
msu-collect -n TEST_NS_CLIENT,TEST_NS_SERVER -o /tmp/msu.cbor

# Inspect a captured file without firing up the analyzer.
msu-collect -dump /tmp/msu.cbor | less
```

The collector is bounded to ~1-2 MB of memory regardless of run length, so
multi-day captures are safe.

## Level 1: Single System MSU Analysis

This is the core workflow that always runs.

### Generate the Report

```bash
$MSU -report <test-dir>
```

This produces `<test-dir>/report.html` — an interactive HTML report with Chart.js.
Open it in a browser. The report contains these chart sections:

1. **CPU %** — Aggregate utilization (iowait=busy and iowait=idle modes) plus per-field breakdown (user, system, softirq, steal, etc.) and per-CPU softirq percentages. Per-CPU series are in the legend but toggled **off** by default — turn them on to see core saturation.
2. **Softirq & Network Rates** — NET_RX/NET_TX softirq events/s (total and per-CPU), softnet processed/drops/squeeze rates.
3. **Interface Traffic & Drops** — Per-interface rx/tx packets/s, Mbps, and drop/error rates from /proc/net/dev.
4. **QEMU & vhost Thread CPU %** — Rendered **only if `qemu-system` processes were seen in `ps auxwww`**. CPU time for qemu-system processes and the associated `vhost-<pid>` kernel threads (which run outside the QEMU cgroup — see `references/qemu-network-overhead.md`).

### Key Questions to Answer

**Is CPU utilization caused by user processes, VMs, or network processing?**
- High **user%** → application workload (user processes or iperf)
- High **system%** on a host with VMs → likely KVM exit handling
- High **softirq%** → network packet processing (interrupt-driven)
- High **steal%** inside a VM → host is overcommitted or host CPU is saturated

**Are any CPU cores saturated?**
- Look at the per-CPU softirq % series in the CPU chart
- Toggle on individual cores — any core at 80%+ non-idle is a potential bottleneck
- For deeper per-core analysis, use the CLI:
  ```bash
  $MSU -command "cat /proc/stat" -section-type B -from <START> -to <END> <msu-file>
  ```

**Are there drops on any network interfaces?**
- Check the Interface Traffic & Drops chart for non-zero drop/error rates
- For detailed interface investigation:
  ```bash
  $MSU -changing "<interface-name>" -section-type A <msu-file>
  ```
- Check softnet drops/squeeze rates — non-zero values indicate kernel packet processing issues

**For targeted CLI investigation:**
```bash
# Track a specific counter over time
$MSU -command "cat /sys/class/net/<dev>/statistics/rx_dropped" -section-type A <msu-file>

# Find all changing counters for an interface
$MSU -changing "<interface-name>" -section-type A -from <START> -to <END> <msu-file>

# CPU stats in a specific time window
$MSU -command "cat /proc/stat" -section-type B -from <START> -to <END> <msu-file>
```

## Level 2: + Zedcloud API Comparison

When Zedcloud API data files are in the same directory, the report automatically
overlays cloud API CPU utilization on the CPU % chart.

### File Naming Convention

```
<system-name>.status.cpu_util.10s    # Tab-separated: timestamp\t\tvalue
<system-name>.status.cpu_util.60s
<system-name>.timeSeries.CPU_USAGE.60s   # JSON with timestamp/values arrays
<system-name>.timeSeries.CPU_TOTAL.60s
```

The `<system-name>` prefix must match the MSU file prefix (e.g., if the MSU file
is `ENODE_TEST_AAAA_jt9_monitor_system_usage.out`, the status file should be
`ENODE_TEST_AAAA_jt9.status.cpu_util.10s`).

### Interpreting Discrepancies

The Zedcloud API CPU utilization often differs from what /proc/stat shows:

- **For the host device**: The API uses a 1-hour averaging window
  (`(last - first) * 100 / 3600`), while /proc/stat shows instantaneous deltas.
  Short spikes may be averaged out.

- **For VMs**: The API derives utilization from `cpuacct.usage` (cgroups v1),
  which measures ALL CPU time of the QEMU process — not just guest vCPU
  execution. This includes KVM exit handling, QEMU userspace I/O, and management
  threads. During network-heavy workloads, the cloud API can report 2-4x higher
  than what /proc/stat shows inside the VM.

Load `references/cpu-utilization-pipeline.md` for the full EVE-OS → Zedcloud → API
data flow details.

## Level 3: + VM-Internal MSU Data

When both host and VM MSU data are in the same directory, the report generates
separate chart panels for each system.

### Cross-Reference Analysis

Compare these across host and VM:

| Host metric | VM metric | What it tells you |
|---|---|---|
| High system% | Low CPU inside VM | KVM exit overhead (virtio kicks, interrupt injection) |
| softirq rates on host | Interface traffic in VM | Network processing cost on the host |
| QEMU thread CPU % | Total VM CPU from cloud API | How much is overhead vs actual guest work |
| vhost thread CPU % | VM softirq rates | Data-plane processing in kernel vs guest |

**EVE-OS already uses vhost-net** (virtio host networking), which moves the data
plane to kernel threads outside the QEMU cgroup. But KVM exits for virtio
kicks/interrupt injection still go through QEMU threads.

Load `references/qemu-network-overhead.md` for the detailed overhead explanation
and reduction strategies (SR-IOV, PCI Passthrough, vDPA).

## Level 4: + iperf Test Data

When iperf test results are available, the report adds rate-step analysis charts
showing how CPU, softirq rates, and packet drops scale with offered load.

### Prepare iperf Data

```bash
python3 SKILL_DIR/scripts/extract_test_params.py <test-dir> --pretty > <test-dir>/params.json
python3 SKILL_DIR/scripts/parse_iperf.py <test-dir> --pretty > <test-dir>/iperf.json
```

For a quick overview of iperf results:
```bash
python3 SKILL_DIR/scripts/parse_iperf.py <test-dir> --output-format table
```

### Generate Full Report

```bash
$MSU -report <test-dir> -iperf-json <test-dir>/iperf.json -params-json <test-dir>/params.json
```

The report now includes a **Rate-Step Analysis** section with:
- **Throughput vs Load** — target kpps vs achieved client/server kpps
- **CPU Usage vs Load** — per-core stacked CPU% for pinned client/server cores
- **SoftIRQ Rate vs Load** — NET_RX/NET_TX events/sec at each rate step

### Root Cause Determination

Use this decision tree to identify the throughput bottleneck:

**Single core ~100% softirq**
Diagnosis: NIC IRQ bottleneck. All interrupts for the NIC go to one CPU.
Evidence: One core >90% softirq, others mostly idle, NET_RX rate plateaus.
Fix: Enable RSS (`ethtool -L <dev> combined N`), distribute IRQ affinity, or enable RPS.

**Single core ~100% user**
Diagnosis: iperf process bottleneck. Single-threaded iperf maxing out its pinned core.
Evidence: One core >90% user%, matches a pinned core from params.
Fix: Use more flows pinned to different cores.

**rx_dropped increasing on interface**
Diagnosis: Ring buffer overflow. Packets arriving faster than kernel can drain.
Evidence: `rx_dropped` or `rx_missed_errors` increasing in sysfs or ethtool -S.
Fix: Increase ring buffer: `ethtool -G <dev> rx 4096`.

**softnet_stat column 2 (dropped) increasing**
Diagnosis: Kernel backlog overflow. `netdev_max_backlog` too small.
Evidence: Non-zero second column in `/proc/net/softnet_stat` (hex).
Fix: `sysctl -w net.core.netdev_max_backlog=10000`.

**softnet_stat column 3 (time_squeeze) increasing**
Diagnosis: SoftIRQ budget exhaustion.
Evidence: Third column increasing, core may show high softirq% but not 100%.
Fix: `sysctl -w net.core.netdev_budget=600` and `net.core.netdev_budget_usecs=4000`.

**iperf loss without kernel drops**
Diagnosis: UDP socket buffer overflow. Server can't read from socket fast enough.
Evidence: iperf reports lost datagrams but all kernel counters are clean.
Fix: `sysctl -w net.core.rmem_max=26214400` or `iperf -s -u -w 16M`.

Load `references/linux-networking-internals.md` for deep dives into the Linux
packet path, softnet_stat columns, RSS/RPS, and tuning parameters.

## Writing the Analysis into the Report

After generating report.html and performing the analysis, you MUST write your
findings into the report's Analysis section. The Go tool generates a placeholder
between `<!-- ANALYSIS_START -->` and `<!-- ANALYSIS_END -->` markers inside
`<div id="analysis">`. Replace that placeholder with your analysis as HTML.

Use the Edit tool to replace the placeholder content. The old_string to match is:
```
    <p class="analysis-placeholder"><!-- ANALYSIS_START -->Analysis pending — run the msu-analyst skill to generate findings.<!-- ANALYSIS_END --></p>
```

Replace it with your analysis HTML. The report already has CSS styles for all the
elements below — just write the HTML directly.

### HTML Elements Available

**Section headings:**
```html
<h2>Executive Summary</h2>
<h3>CPU Utilization Breakdown</h3>
```

**Findings with severity levels** (colored left-border callouts):
```html
<div class="finding">CPU utilization is primarily driven by softirq processing on cpu5.</div>
<div class="warning">softnet_stat squeeze count is increasing — budget may be too low.</div>
<div class="critical">cpu3 is saturated at 98% non-idle, dominated by softirq.</div>
<div class="ok">No packet drops detected on any interface.</div>
```

**Tables** (styled automatically with alternating rows):
```html
<table>
  <tr><th>Core</th><th>user%</th><th>system%</th><th>softirq%</th><th>idle%</th></tr>
  <tr><td>cpu0</td><td>2.1</td><td>3.4</td><td>0.8</td><td>93.7</td></tr>
</table>
```

**ASCII diagrams** (monospace, pre-formatted):
```html
<div class="diagram">
  Host (8 CPUs)                    VM (6 vCPUs)
  ┌─────────────────┐             ┌────────────────┐
  │ /proc/stat      │             │ /proc/stat     │
  │ user:  5%       │  cpuacct    │ user:  8%      │
  │ system: 12% ←───│── 38% ───→ │ softirq: 3%    │
  │ softirq: 8%     │  (overhead) │ idle: 89%      │
  └─────────────────┘             └────────────────┘
</div>
```

**Code/commands:**
```html
<code>ethtool -L eth0 combined 4</code>
<pre>sysctl -w net.core.netdev_budget=600
sysctl -w net.core.netdev_budget_usecs=4000</pre>
```

### Analysis Structure

Write the analysis following this structure (adapt sections based on what data
is available at the current level):

1. **Executive Summary** — 2-3 sentences answering the key questions: What is
   driving CPU usage? Are there bottlenecks? Are there drops?

2. **CPU Utilization Breakdown** — Table of aggregate CPU field percentages
   (averaged over the full data period or key time windows). Identify dominant
   fields. If QEMU threads are present, include their CPU cost.

3. **Per-Core Saturation** — Table of per-core non-idle percentages. Flag any
   cores above 80%. Identify what role they serve (softirq processing, QEMU
   vCPU, user application).

4. **Network Performance** — Interface traffic summary table (rx/tx pps and
   Mbps for each active interface). Drop/error analysis. Softnet drops and
   squeeze counts.

5. **Cloud API Comparison** (Level 2+) — Table comparing /proc/stat CPU % with
   cloud API CPU utilization. Explain the discrepancy. Include an ASCII diagram
   showing the data flow if it helps.

6. **Host vs VM Cross-Reference** (Level 3) — Side-by-side comparison. Diagram
   showing where CPU time is spent (guest vCPU vs KVM exits vs vhost threads
   vs host softirq).

7. **Rate-Step Analysis** (Level 4) — Summary of throughput cap, which rate it
   occurs at, and the bottleneck diagnosis using the decision tree.

8. **Recommendations** — Specific, actionable tuning suggestions with commands.

### Important

- Use data from the charts and CLI investigation — don't just describe what the
  charts show in general terms. Include specific numbers, percentages, and rates.
- For time periods with distinct behavior (e.g., idle → traffic → idle), analyze
  them separately and note the transitions.
- The analysis section is visible in the same HTML page as the charts, so you can
  reference chart names (e.g., "See the ENODE_TEST_AAAA_jt9 CPU % chart").

## Reference Files

| File | Description | When to load |
|------|-------------|--------------|
| `references/msu-tool-reference.md` | msu binary CLI flags, query patterns, output formats | When constructing msu commands |
| `references/linux-networking-internals.md` | Linux packet path, softnet_stat, RSS/RPS, bottleneck patterns | When diagnosing network root causes |
| `references/cpu-utilization-pipeline.md` | EVE-OS CPU collection → Zedcloud storage → API response | When interpreting cloud API vs /proc/stat discrepancy |
| `references/qemu-network-overhead.md` | QEMU/KVM CPU overhead, cpuacct.usage vs guest /proc/stat | When analyzing host-vs-VM CPU differences |
| `scripts/parse_iperf.py` | Parse iperf outputs → JSON | Level 4, Step 1 |
| `scripts/extract_test_params.py` | Parse run.sh → JSON config | Level 4, Step 1 |
