# MSU Tool Reference

The `monitor-system-usage` toolkit has two binaries:

- **`msu-collect`** — long-running collector that samples kernel/sysfs state
  on the device under test and writes structured CBOR records.
- **`msu`** — offline analyzer that reads collected files (CBOR `.msu.cbor`
  or legacy text `*_monitor_system_usage.out`) and produces interactive
  HTML reports or CLI tables. Format is auto-detected.

All examples below use a `$MSU` shell variable to refer to the analyzer
binary. See **Setting `$MSU`** immediately below.

## Setting `$MSU`

The analyzer is built from this repo. Either build it locally and use the
project-relative path:

```bash
go build -o msu ./cmd/msu/
export MSU=./msu          # from the repo root
```

…or use the binary installed by `scripts/install.sh` (which downloads a
release-tagged, checksum-verified asset to `/usr/local/bin`):

```bash
export MSU=/usr/local/bin/msu
```

The skill's command examples assume `$MSU` resolves to a working
analyzer binary in either location.

## Source Layout

`cmd/msu/` is split across several files; useful when grepping:

| File | Contents |
|------|----------|
| `main.go` | Flag parsing, default sections analysis, `-command` and `-changing` modes, text+CBOR parsers. |
| `proc_stat.go` | Per-core CPU and softirq breakdown printed when `-command` targets `/proc/stat`. |
| `net_dev.go` | `/proc/net/dev` parsing for the report's interface chart. |
| `cpu_compare.go` | Multi-system CPU comparison HTML output (`-cpu-compare`). |
| `report.go` | Unified HTML report (`-report`), including iperf rate-step charts. |

`cmd/msu-collect/` is a separate Go binary with its own `main.go`,
`collector.go`, `discover.go`, `sources.go`, `exec.go`. Schema types
live in `pkg/msuformat/`.

---

## `msu-collect` (collector)

`cmd/msu-collect/README.md` is the authoritative reference for the
collector — what it gathers, in which sections, and how the on-disk
schema is shaped. Read it before editing this skill or before
extending the collected data set. The summary below is intentionally
short.

### Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `-interval` | `10` (sec) | Collection interval. B sources fire every interval; A sources every 3rd interval. |
| `-flush-interval` | `6` | Flush+fsync the writer every N intervals (default 60s at a 10s interval). On crash, at most this many intervals are lost. |
| `-n` | (none) | Comma-separated list of network namespaces to also monitor (e.g. `-n TEST_NS_CLIENT,TEST_NS_SERVER`). |
| `-o` | stdout | Output file path. The convention is `.msu.cbor`. |
| `-include-env` | `filtered` | What to record in the header's `env` map: `filtered` drops keys matching `(TOKEN|KEY|SECRET|PASS|AUTH|CRED|COOK)`, `all` keeps everything, `none` omits the map. |
| `-dump` | (none) | Read a CBOR file and print samples as human-readable text on stdout, then exit. Mutually exclusive with collection. |
| `-version` | | Print version and exit. |

### Sections at a glance

- **Init** (once, before the loop): `lscpu`, `lscpu -e`, `lsmem`,
  `dmidecode`, `lspci -vv`, plus `ethtool -i` per discovered interface.
- **A sections** (every 3rd interval, default ~30s): heavy snapshots —
  `ps auxwww`, the `/proc/net/*` family
  (`dev`, `softnet_stat`, `netstat`, `snmp`, `snmp6`, `sockstat`),
  `/proc/{interrupts,softirqs}`, full firewall (`iptables`/`ip6tables`
  filter+nat with counters), `bridge fdb/vlan show`,
  `ip route show table all`, conntrack count/max, and per-interface
  `ip -d -s addr show`, `ethtool -k/-l/-c/-g/-S/--phy-statistics`,
  `tc -s qdisc/class`, plus per-queue sysfs files. Repeated inside
  each namespace passed via `-n`.
- **B sections** (every interval, default 10s): lightweight high-frequency
  state — `/proc/{stat,meminfo,loadavg,vmstat,diskstats,net/softnet_stat}`,
  `/proc/pressure/{cpu,memory,io}` (PSI), discovered cgroup files
  (`cpu.stat`, `memory.stat`, plus v2's `memory.current` / `io.stat`),
  and full per-QEMU coverage: process-level
  (`/proc/<pid>/{stat,statm,status,wchan,sched,schedstat}`), every
  in-process thread (`/proc/<pid>/task/<tid>/{stat,comm}`, including
  the named vCPU threads like `CPU 0/KVM`), AND associated kernel
  threads with their own PIDs (`vhost-<pid>`, `kvm-pit/<pid>`,
  `kvm-nx-lpage-recovery-<pid>`). Kernel-thread coverage is what makes
  vhost CPU attribution possible.

For the full per-source rationale and the CBOR field-by-field schema,
see `cmd/msu-collect/README.md`.

### Common invocations

```bash
# Long-running device capture, writes CBOR to /persist (survives reboots).
msu-collect -interval 10 -o /persist/newlog/keepSentQueue/msu.cbor

# Traffic-generator setup with workload in network namespaces.
msu-collect -n TEST_NS_CLIENT,TEST_NS_SERVER -o /tmp/msu.cbor

# Inspect a captured file without firing up the analyzer.
msu-collect -dump /tmp/msu.cbor | less

# Tighter durability (flush every interval) at the cost of more disk I/O.
msu-collect -flush-interval 1 -o /tmp/msu.cbor
```

---

## CBOR file format (v2)

`pkg/msuformat/types.go` is the source of truth. The on-disk file is a
plain **CBOR sequence** (RFC 8949) — concatenated self-contained CBOR
items with no framing wrapper. Layout:

```
Header
SourceDef(id=0)  Sample(src=0)   ← first time (init/A/B, cmd, ns) tuple appears
Sample(src=0)                    ← subsequent uses just reference the id
SourceDef(id=1)  Sample(src=1)
Sample(src=0)
…
```

Every record carries a `type` discriminator so the reader can dispatch.
`FormatVersion = 2`; v1 is **not** backwards-readable.

### Records

**Header** (one, at start; `type: "header"`):

| Field | Description |
|-------|-------------|
| `v` | Format version (always 2). |
| `ts` | Start time, int64 unix-nanoseconds UTC. |
| `msu_ver` | Collector binary version string. |
| `hz`, `psz` | `CLK_TCK` and `PAGESIZE` (from `getconf`). |
| `cgroup_v` | 1 or 2. |
| `hostname`, `kern_ostype`, `kern_release`, `kern_version` | From `/proc/sys/kernel/*`. |
| `interval_ns`, `flush_every_n` | Sampling cadence. |
| `cmdline` | `os.Args` of the `msu-collect` invocation. |
| `env`, `env_mode` | Filtered/full/none environment, plus the mode. |

**SourceDef** (inline, `type: "src"`): assigns a `uint16` `id` to a
`(sec, cmd, ns)` tuple. Emitted exactly once per tuple, immediately
before the first `Sample` that references it. Subsequent samples for
the same tuple omit `cmd`/`ns`/`sec` and just carry `src: <id>`.

| Field | Description |
|-------|-------------|
| `id` | uint16. |
| `sec` | `"init"`, `"A"`, or `"B"`. |
| `cmd` | Canonical command identifier, e.g. `"cat /proc/stat"` or `"ethtool -S eth0"`. |
| `ns` | Network namespace; omitted in root namespace. |

**Sample** (`type: "s"`):

| Field | Description |
|-------|-------------|
| `ts` | Sample timestamp, int64 unix-nanoseconds UTC. |
| `seq` | Monotonic interval counter. Same `seq` across samples in the same B (or A) sweep — useful for grouping. |
| `src` | uint16, references a previously-emitted `SourceDef.id`. |
| `out` | Raw command/file output, newline-joined. |
| `err` | Error message (omitted on success). |

### Crash safety / partial-write recovery

Because each record is a self-contained CBOR item, a truncated tail
item is detectable. The reader (`pkg/msuformat/reader.go`) treats both
`io.EOF` and `io.ErrUnexpectedEOF` from `cbor.Decoder` as a clean
end-of-stream — see commit `af3cf45`. So a SIGKILL'd `msu-collect`
leaves a file where every fully-flushed record reads back without
error and the last partial record is silently dropped on read.

### Legacy text format

`scripts/monitor_system_usage.sh` (the predecessor shell script) emits
text files with these markers, which `msu` still auto-detects:

- A sections: `++++ BEGIN <id> <ts> ++++` / `++++ END <id> <ts> ++++`
- B sections: `==== BEGIN <id> <ts> ====` / `==== END <id> <ts> ====`
- Commands: `----> <command>` followed by output lines.
- Namespace suffix: `<NS=NAME>` appended to the command name.
- Timestamps: `YYYY_MM_DD_HH_MM_SS`.

All new captures should be CBOR; the text format is read-only support
for older test bundles.

---

## `msu` (analyzer)

### Flags

#### Default sections analysis

| Flag | Description |
|------|-------------|
| `-command "CMD"` | Track a specific command across sections. |
| `-changing "PATTERN"` | Show all commands whose name contains PATTERN and whose value changes. Mutually exclusive with `-command`. |
| `-section-type A\|B\|both` | Restrict to A sections, B sections, or both (default). |
| `-field-pattern "REGEX"` | Only match lines matching this regex within the command's output. |
| `-value-pattern "REGEX"` | Capture-group regex to extract the numeric value (default `(\d+\.?\d*)`). |
| `-from "TIME"`, `-to "TIME"` | Time window. Accepts `15:04:05`, `2006-01-02T15:04:05`, `2006-01-02 15:04:05`, `2006_01_02_15_04_05`. Time-only formats use the first section's date as reference. |
| `-show-a-sections`, `-show-b-sections` | Default true; pass `=false` to suppress. |

#### Report mode

| Flag | Description |
|------|-------------|
| `-report DIR` | Generate `DIR/report.html` from MSU data found in DIR. |
| `-iperf-json PATH` | iperf JSON (from `parse_iperf.py`) for rate-step charts. |
| `-params-json PATH` | Test params JSON (from `extract_test_params.py`) for rate-step charts. |

`-iperf-json` and `-params-json` require `-report`. Without them the
report still includes Levels 1-3.

#### CPU comparison mode

| Flag | Description |
|------|-------------|
| `-cpu-compare DIR` | Generate `DIR/cpu_comparison.html` overlaying CPU data from multiple systems and any matching cloud-API files. |

`-cpu-compare` is a strict subset of `-report` (CPU + softirq + softnet
+ QEMU charts, no interface traffic, no iperf rate-step). Prefer
`-report` for new work; keep `-cpu-compare` in mind only when you
specifically want the narrower output.

### Key usage patterns

**CPU and softirq analysis** — when `-command` targets `/proc/stat`,
`msu` auto-switches to the per-core breakdown formatter
(`proc_stat.go`):

```bash
$MSU -command "cat /proc/stat" -section-type B <msu-file>
$MSU -command "cat /proc/stat" -section-type B -from 12:55:36 -to 12:55:56 <msu-file>
```

Output lists per-CPU usage by field (`user`, `nice`, `system`, `idle`,
`iowait`, `irq`, `softirq`, `steal`, `guest`, `guest_nice`) plus
softirq event rates between consecutive snapshots. Values are
ANSI-colored:

- For the **non-idle fields** (everything except `idle`): yellow >20%,
  orange >50%, red >80%.
- For the **`idle` field** the thresholds are **inverted** (low idle is
  bad): yellow ≤80%, orange ≤50%, red ≤20%.

**Interface counter tracking:**

```bash
# Track a specific counter
$MSU -command "cat /sys/class/net/eth0/statistics/rx_packets" -section-type A <msu-file>

# Track drops
$MSU -command "cat /sys/class/net/eth0/statistics/rx_dropped" -section-type A <msu-file>

# With namespace suffix (legacy text format only — CBOR uses the ns field)
$MSU -command "cat /sys/class/net/enp4s0f0/statistics/rx_dropped <NS=TEST_NS_CLIENT>" -section-type A <msu-file>
```

**Counter discovery** — finds every command whose name contains the
pattern and whose value changes over the file:

```bash
$MSU -changing "eth0" -section-type A <msu-file>
$MSU -changing "enp4s0f0" -section-type A -from 12:57:56 -to 12:58:16 <msu-file>
```

**Softnet stat tracking** — `field-pattern` filters to the per-CPU
hex rows:

```bash
$MSU -command "cat /proc/net/softnet_stat" -section-type A -field-pattern "^[0-9a-f]" <msu-file>
```

**Report generation:**

```bash
# Levels 1-3 (auto-discovers cloud API data in the same directory)
$MSU -report <test-dir>

# Level 4 (adds iperf rate-step analysis)
$MSU -report <test-dir> -iperf-json <test-dir>/iperf.json -params-json <test-dir>/params.json
```

Output: `<test-dir>/report.html` — interactive Chart.js HTML with
synchronized zoom/pan. The analysis placeholder between
`<!-- ANALYSIS_START -->` and `<!-- ANALYSIS_END -->` is meant to be
replaced by the analyst (see `SKILL.md`).

### Output formats

**Default (no `-command`, no `-changing`)** — a section-listing table:

```
"A" sections analysis
=======================
No   ID         Begin Time           End Time             Duration
--------------------------------------------------------------------------------
0    123        2026-04-30 13:14:04  2026-04-30 13:14:10  6s
…
```

For CBOR inputs, the section ID column shows the `seq` counter, since
the v2 format groups samples by sequence rather than by explicit
BEGIN/END markers.

**`-command`** — for non-`/proc/stat` commands, prints first/last
values plus a per-sample table with rate-of-change column. For
`/proc/stat`, dispatches to the per-core breakdown formatter described
above.

**`-changing`** — sorted table of commands by absolute total change:

```
COMMAND                                                      SAMPLES  TOTAL_CHANGE     RATE/s
cat /sys/class/net/eth0/statistics/rx_packets                     50     5000000.00  100000.00
…
```

**`-report`** / **`-cpu-compare`** — interactive HTML with Chart.js.
The unified report contains, in order:

1. Per system: a CPU % chart (aggregate `iowait=busy` and `iowait=idle`,
   per-field breakdown, and per-CPU softirq% — the per-CPU softirq
   series are in the legend but toggled off by default).
2. Per system: a Softirq & Network Rates chart
   (NET_RX/NET_TX events/s total + per-CPU; softnet processed/drops/squeeze
   total + per-CPU).
3. Per system: an Interface Traffic & Drops chart from `/proc/net/dev`.
4. Per system: a QEMU & vhost Thread CPU % chart — **rendered only if
   `qemu-system` processes were seen in the `ps auxwww` output**.
5. (If `-iperf-json`/`-params-json` provided) rate-step charts:
   Throughput vs Load, CPU Usage vs Load (Key Cores), SoftIRQ Rate vs
   Load.
6. An empty Analysis section for the analyst to fill in.

If cloud-API files (`*.status.cpu_util.{10s,60s}`,
`*.timeSeries.CPU_{USAGE,TOTAL}.*`) are present in the report
directory, their series are overlaid on each system's CPU chart.
