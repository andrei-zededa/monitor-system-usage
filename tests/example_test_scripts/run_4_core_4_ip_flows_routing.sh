#!/bin/sh

set -eu;

CNS="TEST_NS_CLIENT";	# CLIENT network namespace
CIF="enp4s0f0";		# CLIENT interface
CCORE="1";		# CLIENT CPU core number for taskset
SNS="TEST_NS_SERVER";	# SERVER network namespace
SIF="enp5s0";		# SERVER interface
SCORE="6";		# SERVER CPU core number for taskset

MSU="msu-collect";
ip="sudo ip";

$ip netns del "$CNS" || :
$ip netns del "$SNS" || :

lscpu >lscpu.out 2>lscpu.err;
lscpu -e >lscpu-e.out 2>lscpu-e.err;
lspci >lscpi.out 2>lscpi.err;
lspci -tv >lscpi-tv.out 2>lscpi-tv.err;
lspci -vv >lscpi-vv.out 2>lscpi-vv.err;
sudo lshw >lshw.out 2>lshw.err;

$ip netns add "$CNS";
$ip netns add "$SNS";

$ip netns exec "$CNS" ip link set lo up;
$ip netns exec "$SNS" ip link set lo up;

$ip link set "$CIF" netns "$CNS";
$ip link set "$SIF" netns "$SNS";

$ip netns exec "$CNS" ip link set "$CIF" up;
$ip netns exec "$SNS" ip link set "$SIF" up;

$ip netns exec "$CNS" ip addr add 10.66.6.100/24 dev "$CIF" label "${CIF}:0";
$ip netns exec "$CNS" ip addr add 10.66.6.101/24 dev "$CIF" label "${CIF}:1";
$ip netns exec "$CNS" ip addr add 10.66.6.102/24 dev "$CIF" label "${CIF}:2";
$ip netns exec "$CNS" ip addr add 10.66.6.103/24 dev "$CIF" label "${CIF}:3";
$ip netns exec "$CNS" ip route add 10.99.9.0/24 via 10.66.6.1 dev "$CIF";

$ip netns exec "$SNS" ip addr add 10.99.9.200/24 dev "$SIF" label "${SIF}:0"; 
$ip netns exec "$SNS" ip addr add 10.99.9.201/24 dev "$SIF" label "${SIF}:1"; 
$ip netns exec "$SNS" ip addr add 10.99.9.202/24 dev "$SIF" label "${SIF}:2"; 
$ip netns exec "$SNS" ip addr add 10.99.9.203/24 dev "$SIF" label "${SIF}:3"; 
$ip netns exec "$SNS" ip route add 10.66.6.0/24 via 10.99.9.1 dev "$SIF";

sleep 10s;
$ip netns exec "$CNS" ping -c 3 -I 10.66.6.100 10.99.9.200;
$ip netns exec "$CNS" ping -c 3 -I 10.66.6.101 10.99.9.201;
$ip netns exec "$CNS" ping -c 3 -I 10.66.6.102 10.99.9.202;
$ip netns exec "$CNS" ping -c 3 -I 10.66.6.103 10.99.9.203;

sudo $MSU -n "${CNS},${SNS}" >msu.out 2>msu.err &
MSU_PID="$!";

( date; $ip netns exec "$SNS" taskset -c "$SCORE" iperf -s -B 10.99.9.200 -u; date ) >server_0_iperf.out 2>server_0_iperf.err &
SERVER_0_PID="$!";
( date; $ip netns exec "$SNS" taskset -c "$(( SCORE + 1 ))" iperf -s -B 10.99.9.201 -u; date ) >server_1_iperf.out 2>server_1_iperf.err &
SERVER_1_PID="$!";
( date; $ip netns exec "$SNS" taskset -c "$(( SCORE + 2 ))" iperf -s -B 10.99.9.202 -u; date ) >server_2_iperf.out 2>server_2_iperf.err &
SERVER_2_PID="$!";
( date; $ip netns exec "$SNS" taskset -c "10" iperf -s -B 10.99.9.203 -u; date ) >server_3_iperf.out 2>server_3_iperf.err &
SERVER_3_PID="$!";

TLEN="60"; # Test length in seconds.
sleep "${TLEN}s";

CRATE="100"; # CLIENT kpps
for CRATE in 100 200 300 400 500 600 700 800 900 1000; do
	(
		date;
		for i in $(seq 1 1); do
			subrate="$(( CRATE / 4 ))";
			$ip netns exec "$CNS" taskset -c "$(( CCORE + 0 ))" iperf -u -B 10.66.6.100 -c 10.99.9.200 -b "${subrate}kpps" -l 100 -t "$TLEN" >>"client_iperf_${CRATE}_${subrate}_0.out" 2>>"client_iperf_${CRATE}_${subrate}_0.err" &
			c_0_pid="$!";
			$ip netns exec "$CNS" taskset -c "$(( CCORE + 1 ))" iperf -u -B 10.66.6.101 -c 10.99.9.201 -b "${subrate}kpps" -l 100 -t "$TLEN" >>"client_iperf_${CRATE}_${subrate}_1.out" 2>>"client_iperf_${CRATE}_${subrate}_1.err" &
			c_1_pid="$!";
			$ip netns exec "$CNS" taskset -c "$(( CCORE + 2 ))" iperf -u -B 10.66.6.102 -c 10.99.9.202 -b "${subrate}kpps" -l 100 -t "$TLEN" >>"client_iperf_${CRATE}_${subrate}_2.out" 2>>"client_iperf_${CRATE}_${subrate}_2.err" &
			c_2_pid="$!";
			$ip netns exec "$CNS" taskset -c "$(( CCORE + 3 ))" iperf -u -B 10.66.6.103 -c 10.99.9.203 -b "${subrate}kpps" -l 100 -t "$TLEN" >>"client_iperf_${CRATE}_${subrate}_3.out" 2>>"client_iperf_${CRATE}_${subrate}_3.err" &
			c_3_pid="$!";
			echo "Wait for tests to finish and the sleep and extra $(( TLEN / 1 ))s.....";
			wait "$c_0_pid" "$c_1_pid" "$c_2_pid" "$c_3_pid";
			sleep "$(( TLEN / 1 ))s";
		done;
		date;
	) >"client_iperf_$CRATE.out" 2>"client_iperf_$CRATE.err";
done

sudo pkill -f iperf || :
sudo pkill -9 -f iperf || :
sudo pkill -f monitor_system_usage || :
sudo pkill -f msu || :
sudo pkill -9 -f monitor_system_usage || :
sudo pkill -9 -f msu || :

sudo kill "$MSU_PID" || :
sudo kill -9 "$MSU_PID" || :

wait "$MSU_PID";
