#!/bin/sh

set -eu;

CNS="TEST_NS_B";	# CLIENT network namespace
CIF="tapB-SV-jt9";	# CLIENT interface
CCORE="0";		# CLIENT CPU core number for taskset
SNS="TEST_NS_A";	# SERVER network namespace
SIF="tapA-SV-jt9";	# SERVER interface
SCORE="4";		# SERVER CPU core number for taskset

ip="sudo ip";

$ip netns exec "$CNS" ip addr add 10.99.2.200/24 dev "$CIF" label "${CIF}:0";
$ip netns exec "$CNS" ip addr add 10.99.2.201/24 dev "$CIF" label "${CIF}:1";
$ip netns exec "$CNS" ip addr add 10.99.2.202/24 dev "$CIF" label "${CIF}:2";
$ip netns exec "$CNS" ip addr add 10.99.2.203/24 dev "$CIF" label "${CIF}:3";

$ip netns exec "$SNS" ip addr add 10.99.1.100/24 dev "$SIF" label "${SIF}:0"; 
$ip netns exec "$SNS" ip addr add 10.99.1.101/24 dev "$SIF" label "${SIF}:1"; 
$ip netns exec "$SNS" ip addr add 10.99.1.102/24 dev "$SIF" label "${SIF}:2"; 
$ip netns exec "$SNS" ip addr add 10.99.1.103/24 dev "$SIF" label "${SIF}:3"; 

sleep 10s;
$ip netns exec "$CNS" ping -c 3 -I 10.99.2.200 10.99.1.100;
$ip netns exec "$CNS" ping -c 3 -I 10.99.2.201 10.99.1.101;
$ip netns exec "$CNS" ping -c 3 -I 10.99.2.202 10.99.1.102;
$ip netns exec "$CNS" ping -c 3 -I 10.99.2.203 10.99.1.103;

( date; $ip netns exec "$SNS" taskset -c "$SCORE" iperf -s -B 10.99.1.100 -u; date ) >server_0_iperf.out 2>server_0_iperf.err &
SERVER_0_PID="$!";
( date; $ip netns exec "$SNS" taskset -c "$(( SCORE + 1 ))" iperf -s -B 10.99.1.101 -u; date ) >server_1_iperf.out 2>server_1_iperf.err &
SERVER_1_PID="$!";
( date; $ip netns exec "$SNS" taskset -c "$(( SCORE + 2 ))" iperf -s -B 10.99.1.102 -u; date ) >server_2_iperf.out 2>server_2_iperf.err &
SERVER_2_PID="$!";
( date; $ip netns exec "$SNS" taskset -c "$(( SCORE + 3 ))" iperf -s -B 10.99.1.103 -u; date ) >server_3_iperf.out 2>server_3_iperf.err &
SERVER_3_PID="$!";

TLEN="30"; # Test length in seconds.
sleep "${TLEN}s";

CRATE="100"; # CLIENT kpps
for CRATE in 10 20 40 80 100 160 220 280; do
	(
		date;
		for i in $(seq 1 1); do
			subrate="$(( CRATE / 4 ))";
			$ip netns exec "$CNS" taskset -c "$(( CCORE + 0 ))" iperf -u -B 10.99.2.200 -c 10.99.1.100 -b "${subrate}kpps" -l 20 -t "$TLEN" >>"client_iperf_${CRATE}_${subrate}_0.out" 2>>"client_iperf_${CRATE}_${subrate}_0.err" &
			c_0_pid="$!";
			$ip netns exec "$CNS" taskset -c "$(( CCORE + 1 ))" iperf -u -B 10.99.2.201 -c 10.99.1.101 -b "${subrate}kpps" -l 20 -t "$TLEN" >>"client_iperf_${CRATE}_${subrate}_1.out" 2>>"client_iperf_${CRATE}_${subrate}_1.err" &
			c_1_pid="$!";
			$ip netns exec "$CNS" taskset -c "$(( CCORE + 2 ))" iperf -u -B 10.99.2.202 -c 10.99.1.102 -b "${subrate}kpps" -l 20 -t "$TLEN" >>"client_iperf_${CRATE}_${subrate}_2.out" 2>>"client_iperf_${CRATE}_${subrate}_2.err" &
			c_2_pid="$!";
			$ip netns exec "$CNS" taskset -c "$(( CCORE + 3 ))" iperf -u -B 10.99.2.203 -c 10.99.1.103 -b "${subrate}kpps" -l 20 -t "$TLEN" >>"client_iperf_${CRATE}_${subrate}_3.out" 2>>"client_iperf_${CRATE}_${subrate}_3.err" &
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
