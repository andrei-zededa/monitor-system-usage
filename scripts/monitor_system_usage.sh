#!/bin/sh
# shellcheck disable=SC2044

set -eu;

MSU_VERSION="0.0.4";

DATE_FMT="+%Y_%m_%d_%H_%M_%S";

dump_file() {
	n="";
	f="";

	[ "_${1:-}" = "_-n" ] && {
		n="${2:-}";
		shift; shift;
	}
	f="${1:-}";
	[ -z "$f" ] && return;


	[ -n "$n" ] && {
		OLD_IFS="$IFS";
		IFS=",";
		for ns in $n; do
			IFS="$OLD_IFS";
			printf "\n----------------------------> cat %s <NS=$ns>\n" "$f";
			ip netns exec "$ns" cat "$f" || true;
		done
		IFS="$OLD_IFS";

		return;
	}

	printf "\n----------------------------> cat %s\n" "$f";
	cat "$f" || true;
}

run_cmd() {
	n="";
	[ "_${1:-}" = "_-n" ] && {
		n="${2:-}";
		shift; shift;
	}

	[ -n "$n" ] && {
		OLD_IFS="$IFS";
		IFS=",";
		for ns in $n; do
			IFS="$OLD_IFS";
			printf "\n----------------------------> %s <NS=$ns>\n" "$*";

			ip netns exec "$ns" "$@" || true;
		done
		IFS="$OLD_IFS";

		return;
	}

	printf "\n----------------------------> %s\n" "$*";
	"$@" || true;
}

INTERVAL="10"; # seconds.
NOW="$(date "$DATE_FMT")";
NETNSES="";

printf "MSU version=%s starting at date=%s interval=%sseconds\n" "$MSU_VERSION" "$NOW" "$INTERVAL";

# Script can be called with $0 -n COMMA,SEPARATED,LIST,OF,NETWORK,NAMESPACES .
[ "_${1:-}" = "_-n" ] && {
	NETNSES="${2:-}";
	[ -z "$NETNSES" ] && {
		>&2 printf "Error: -n specified but no list of network namespaces.\n";
		>&2 printf "Usage: %s [-n COMMA,SEPARATED,LIST,OF,NETWORK,NAMESPACES]\n" "$0";
		exit 1;
	}
}

apk add --no-cache musl-utils iproute2 ethtool || {
	printf "Can't install tools with 'apk' (Not running inside 'eve debug' container ? Or maybe not running on EVE-OS at all ?).\n";
};

HZ="$(getconf CLK_TCK)";
PSZ="$(getconf PAGESIZE)";
QEMUZ="$(pgrep -f qemu-system-x86_64 | awk '{printf("%s ", $1);}')";

# Detect cgroup version once before the loop (item 12).
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
	CGROUP_V2="1";
else
	CGROUP_V2="";
fi

# Driver info per interface — doesn't change at runtime (item 14).
for path in $(find /sys/class/net -mindepth 1 -maxdepth 1); do
	[ "_$path" = "_" ] || [ "_$path" = "_." ] || [ "_$path" = "_./" ] || [ "_$path" = "_/" ] && continue;
	intf="$(basename "$path")";
	run_cmd ethtool -i "$intf";
done

i="0";
while true; do
	# "A section" (++++ BEGIN .... ++++ END) runs every 3rd interval. Can
	# also run every interval if needed.
	[ $(( i % 3 )) -eq 0 ] && {
	# true && {
		NOW="$(date "$DATE_FMT")";
		printf "\n++++ BEGIN %s %s ++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++\n" "$i" "$NOW";
		HZ="$(getconf CLK_TCK)";
		printf "\n----------------------------> HZ=%s\n" "$HZ";
		PSZ="$(getconf PAGESIZE)";
		printf "\n----------------------------> PSZ=%s\n" "$PSZ";
		printf "\n----------------------------> ps auxwww\n";
		ps auxww;
		QEMUZ="$(pgrep -x qemu-system | awk '{printf("%s ", $1);}')";
		printf "\n----------------------------> QEMUZ=%s\n" "$QEMUZ";
		NOW="$(date "$DATE_FMT")";

		dump_file "/proc/interrupts";
		dump_file "/proc/softirqs";

		dump_file "/proc/net/dev";
		dump_file "/proc/net/softnet_stat";
		dump_file "/proc/net/netstat";
		dump_file "/proc/net/snmp";
		dump_file "/proc/net/snmp6";
		dump_file "/proc/net/sockstat";

		run_cmd iptables -vnL;
		run_cmd iptables -t nat -vnL;
		run_cmd ip6tables -vnL;
		run_cmd ip6tables -t nat -vnL;

		dump_file "/proc/sys/net/netfilter/nf_conntrack_count";
		dump_file "/proc/sys/net/netfilter/nf_conntrack_max";

		run_cmd bridge fdb show;
		run_cmd bridge vlan show;
		run_cmd ip route show table all;

		for path in $(find /sys/class/net -mindepth 1 -maxdepth 1); do
			[ "_$path" = "_" ] || [ "_$path" = "_." ] || [ "_$path" = "_./" ] || [ "_$path" = "_/" ] && continue; 
			intf="$(basename "$path")";
			run_cmd ip -d -s addr show "$intf";
			run_cmd ethtool -k "$intf";
			run_cmd ethtool -l "$intf";
			run_cmd ethtool -c "$intf";
			run_cmd ethtool -g "$intf";
			run_cmd ethtool -S "$intf";
			run_cmd ethtool --phy-statistics "$intf";
			run_cmd tc -s qdisc show dev "$intf";
			run_cmd tc -s class show dev "$intf";
			for f in $(find "${path}/statistics" -mindepth 1 -maxdepth 1); do
				[ "_$f" = "_" ] || [ "_$f" = "_." ] || [ "_$f" = "_./" ] || [ "_$f" = "_/" ] && continue; 
				dump_file "$f";
			done
			for f in $(find "${path}/queues/" -type f); do
				[ "_$f" = "_" ] || [ "_$f" = "_." ] || [ "_$f" = "_./" ] || [ "_$f" = "_/" ] && continue; 
				dump_file "$f";
			done
		done

		OLD_IFS="$IFS";
		IFS=",";
		for NS in $NETNSES; do
			IFS="$OLD_IFS";
			dump_file -n "$NS" "/proc/net/dev";
			dump_file -n "$NS" "/proc/net/softnet_stat";
			dump_file -n "$NS" "/proc/net/netstat";
			dump_file -n "$NS" "/proc/net/snmp";
			dump_file -n "$NS" "/proc/net/snmp6";
			dump_file -n "$NS" "/proc/net/sockstat";
			dump_file -n "$NS" "/proc/softirqs";

			run_cmd -n "$NS" iptables -vnL;
			run_cmd -n "$NS" iptables -t nat -vnL;
			run_cmd -n "$NS" ip6tables -vnL;
			run_cmd -n "$NS" ip6tables -t nat -vnL;

			for path in $(ip netns exec "$NS" find /sys/class/net -mindepth 1 -maxdepth 1); do
				[ "_$path" = "_" ] || [ "_$path" = "_." ] || [ "_$path" = "_./" ] || [ "_$path" = "_/" ] && continue; 
				intf="$(basename "$path")";
				run_cmd -n "$NS" ip -d -s addr show "$intf";
				run_cmd -n "$NS" ethtool -k "$intf";
				run_cmd -n "$NS" ethtool -l "$intf";
				run_cmd -n "$NS" ethtool -c "$intf";
				run_cmd -n "$NS" ethtool -g "$intf";
				run_cmd -n "$NS" ethtool -S "$intf";
				run_cmd -n "$NS" ethtool --phy-statistics "$intf";
				run_cmd -n "$NS" tc -s qdisc show dev "$intf";
				run_cmd -n "$NS" tc -s class show dev "$intf";
				for f in $(ip netns exec "$NS" find "${path}/statistics" -mindepth 1 -maxdepth 1); do
					[ "_$f" = "_" ] || [ "_$f" = "_." ] || [ "_$f" = "_./" ] || [ "_$f" = "_/" ] && continue; 
					dump_file -n "$NS" "$f";
				done
				for f in $(ip netns exec "$NS" find "${path}/queues/" -type f); do
					[ "_$f" = "_" ] || [ "_$f" = "_." ] || [ "_$f" = "_./" ] || [ "_$f" = "_/" ] && continue; 
					dump_file -n "$NS" "$f";
				done
		done

		done
		IFS="$OLD_IFS";

		printf "\n++++ END   %s %s ++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++\n" "$i" "$NOW";
	}

	# "B section" (==== BEGIN .... ==== END) runs every interval.
	NOW="$(date "$DATE_FMT")";
	printf "\n==== BEGIN  %s %s ========================================================================\n" "$i" "$NOW";
	dump_file "/proc/stat";
	dump_file "/proc/meminfo";
	dump_file "/proc/loadavg";
	dump_file "/proc/net/softnet_stat";
	dump_file "/proc/pressure/cpu";
	if [ -n "$CGROUP_V2" ]; then
		for f in $(find /sys/fs/cgroup/ -type f -name cpu.stat); do
			dump_file "$f";
		done
	else
		for f in $(find /sys/fs/cgroup/cpu/ -type f -name cpu.stat); do
			[ "_$f" = "_" ] || [ "_$f" = "_." ] || [ "_$f" = "_./" ] || [ "_$f" = "_/" ] && continue;
			dump_file "$f";
		done
	fi
	dump_file "/proc/vmstat";
	dump_file "/proc/pressure/memory";
	if [ -n "$CGROUP_V2" ]; then
		for f in $(find /sys/fs/cgroup/ -type f -name memory.stat); do
			dump_file "$f";
		done
		for f in $(find /sys/fs/cgroup/ -type f -name memory.current); do
			dump_file "$f";
		done
		for f in $(find /sys/fs/cgroup/ -type f -name io.stat); do
			dump_file "$f";
		done
	else
		for f in $(find /sys/fs/cgroup/memory/ -type f -name memory.stat); do
			[ "_$f" = "_" ] || [ "_$f" = "_." ] || [ "_$f" = "_./" ] || [ "_$f" = "_/" ] && continue;
			dump_file "$f";
		done
	fi
	dump_file "/proc/diskstats";
	dump_file "/proc/pressure/io";
	for p in $QEMUZ; do
		dump_file "/proc/$p/stat";
		dump_file "/proc/$p/statm";
		dump_file "/proc/$p/status";
		dump_file "/proc/$p/wchan";
		dump_file "/proc/$p/sched";
		dump_file "/proc/$p/schedstat";
		for t in "/proc/$p/task"/*; do
			dump_file "$t/stat";
		done
	done
	NOW="$(date "$DATE_FMT")";
	printf "\n==== END   %s %s ========================================================================\n" "$i" "$NOW";
	i="$(( i + 1 ))";
	sleep "$INTERVAL";
done

# | gzip -c > "${1:-/persist/newlog/keepSentQueue}/monitor_system_usage.$(date "$DATE_FMT").log.gz";

# eve enter debug
# /root/monitor_system_usage.sh 1>/persist/newlog/keepSentQueue/monitor_system_usage.log 2>/persist/newlog/keepSentQueue/monitor_system_usage.err &
