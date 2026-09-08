## How it's organized

`msu-collect` records every data point as a CBOR sample tagged with `section`, `cmd` (canonical string like `cat /proc/stat`), `ns`, `seq`, timestamp, stdout, and an error string. Three sections (`pkg/msucollect/collector.go:190`):

| Section | Cadence                                          | Meaning                          |
| ------- | ------------------------------------------------ | -------------------------------- |
| `init`  | Once, before the loop (`seq=0`)                  | Static inventory + fio benchmark |
| `A`     | Every 3rd interval (`seq%3==0`) → 30s at default | Network/config-heavy, expensive  |
| `B`     | Every interval → 10s default                     | CPU/mem/IO counters              |

Defaults: `-interval 10`, `-flush-interval 6` (disk flush every 60s), `-n` namespace list empty.

## Header (written once, not a command)

From `/proc/sys/kernel/{hostname,ostype,osrelease,version}`, plus `getconf CLK_TCK` (HZ), `getconf PAGESIZE` (PSZ), detected cgroup version, interval, flush-every-N, the collector's own argv, and environment variables — `filtered` by default, dropping names matching `TOKEN|KEY|SECRET|PASS|AUTH|CRED|COOK` (`run.go:17`).

## `init` section — once at startup

```
apk add --no-cache musl-utils iproute2 ethtool fio   (best-effort, side effect only, not recorded)
lscpu
lscpu -e
lsmem
dmidecode
lspci -vv
sh -c '<fio 60s 4k randwrite against /persist/vault>'
sh -c '<fio 60s 4k randwrite against $HOME>'
ethtool -i <intf>                                    (per interface in /sys/class/net)
```

## `A` section — every 3rd interval

Static (18 sources, `sources.go:53`):
```
ps auxwww
cat /proc/interrupts
cat /proc/softirqs
cat /proc/net/dev
cat /proc/net/softnet_stat
cat /proc/net/netstat
cat /proc/net/snmp
cat /proc/net/snmp6
cat /proc/net/sockstat
iptables -vnL
iptables -t nat -vnL
ip6tables -vnL
ip6tables -t nat -vnL
cat /proc/sys/net/netfilter/nf_conntrack_count
cat /proc/sys/net/netfilter/nf_conntrack_max
bridge fdb show
bridge vlan show
ip route show table all
```

Per interface (re-discovered from `/sys/class/net` each A section):
```
ip -d -s addr show <intf>
ethtool -k <intf>          (offloads)
ethtool -l <intf>          (channels)
ethtool -c <intf>          (coalescing)
ethtool -g <intf>          (ring sizes)
ethtool -S <intf>          (driver stats)
ethtool --phy-statistics <intf>
tc -s qdisc show dev <intf>
tc -s class show dev <intf>
cat /sys/class/net/<intf>/statistics/*        (every file)
cat /sys/class/net/<intf>/queues/**           (every file, recursive — rx/tx per-queue)
```

Per namespace given with `-n` (all via `ip netns exec <ns> …`):
```
cat /proc/net/dev, softnet_stat, netstat, snmp, snmp6, sockstat, /proc/softirqs
iptables -vnL, iptables -t nat -vnL, ip6tables -vnL, ip6tables -t nat -vnL
```
plus the same 9 per-interface commands for each interface found inside the namespace.

## `B` section — every interval

Static (9 sources):
```
cat /proc/stat
cat /proc/meminfo
cat /proc/loadavg
cat /proc/net/softnet_stat
cat /proc/pressure/cpu
cat /proc/vmstat
cat /proc/pressure/memory
cat /proc/diskstats
cat /proc/pressure/io
```

cgroups (discovered by tree walk each A section):
- v2: every `cpu.stat`, `memory.stat`, `memory.current`, `io.stat` under `/sys/fs/cgroup`
- v1: every `cpu.stat` under `/sys/fs/cgroup/cpu`, every `memory.stat` under `/sys/fs/cgroup/memory`

Per QEMU process (`pgrep -x qemu-system`):
```
cat /proc/<pid>/{stat,statm,status,wchan,sched,schedstat}
cat /proc/<pid>/task/<tid>/{stat,comm}     (every thread — vCPU threads)
cat /proc/<kpid>/{stat,comm}               (kernel threads whose comm contains the QEMU pid:
                                            vhost-<pid>, kvm-pit/<pid>)
```

## Three things worth knowing

**Startup latency is ~2 minutes.** Both fio benchmarks are in `init`, each `--runtime=60 --time_based`, and they run *before* the sampling loop. The `/persist/vault` one is guarded by a directory test, but the `$HOME` one is unconditional — so even on EVE-OS you pay 60s, and off EVE-OS you pay 60s plus an error-recording attempt on the vault script. Any capture is preceded by a minute of synchronous 4k random-write disk load, which will show up in the first samples' `diskstats`/`pressure/io` if the loop overlaps at all.

**Namespace sysfs is intentionally omitted.** Per-interface `statistics/` and `queues/` files are collected only in the root namespace; `sources.go:112` documents this as a deliberate gap versus the shell script, which used `ip netns exec … find`. So NS interface counters come only from `ethtool -S` and `/proc/net/dev`.

> TODO: Double-check the NS vs. interface counters statement, I think that sysfs files are present for all interfaces even those in non-default NSes.

**The Go collector is a superset of the shell script except for HZ/PSZ.** It adds `lscpu/lsmem/dmidecode/lspci`, fio, `task/*/comm`, and the vhost/kvm-pit kernel threads (the script only dumped `task/*/stat`). But `collectASection` refreshes `c.hz`/`c.psz` every A section into fields that are only ever emitted in the already-written header (`collector.go:154`) — those two `getconf` calls are dead work, and unlike the script you get no per-interval record of them. Harmless unless you ever expect HZ to change mid-capture.
