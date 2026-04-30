#!/usr/bin/env python3
"""
Parse iperf client output files from a test directory.

Handles both single-flow and multi-flow test layouts:
  - Single-flow: client_iperf_RATE.out
  - Multi-flow:  client_iperf_RATE_SUBRATE_FLOWID.out  (per-flow files)
                 client_iperf_RATE.out                  (wrapper with timestamps)

Usage:
    parse_iperf.py <test-directory>
    parse_iperf.py <test-directory> --pretty
    parse_iperf.py <test-directory> --output-format table
"""

import json
import re
import sys
from datetime import datetime
from pathlib import Path


# Regex patterns for iperf output parsing
# Client bandwidth: [  1] 0.0000-10.0001 sec  95.4 MBytes  80.0 Mbits/sec
CLIENT_BW_RE = re.compile(
    r'\[\s*\d+\]\s+[\d.]+-[\d.]+\s+sec\s+'
    r'([\d.]+)\s+([KMGT]?)Bytes\s+'
    r'([\d.]+)\s+([KMGT]?)bits/sec'
)

# Sent datagrams: [  1] Sent 1000009 datagrams
SENT_RE = re.compile(r'\[\s*\d+\]\s+Sent\s+(\d+)\s+datagrams')

# Server Report line with jitter and loss:
# [  1] 0.0000-10.0001 sec  95.0 MBytes  79.7 Mbits/sec  0.000 ms 3479/1000008 (0%)
SERVER_REPORT_RE = re.compile(
    r'\[\s*\d+\]\s+[\d.]+-[\d.]+\s+sec\s+'
    r'([\d.]+)\s+([KMGT]?)Bytes\s+'
    r'([\d.]+)\s+([KMGT]?)bits/sec\s+'
    r'([\d.]+)\s+ms\s+'
    r'(\d+)/(\d+)\s+\(([\d.e+-]+)%?\)'
)

# Timestamp line: Thu Feb 12 12:55:36 PM EET 2026
# Python's strptime %Z doesn't reliably parse timezone abbreviations like EET,
# so we use regex to extract components and strip the timezone.
TIMESTAMP_LINE_RE = re.compile(
    r'^(\w+)\s+(\w+)\s+(\d+)\s+(\d+:\d+:\d+)\s+(AM|PM)\s+\w+\s+(\d{4})$'
)

MULTIPLIERS = {"": 1, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12}


def parse_size(value_str, prefix):
    """Convert a value with SI prefix to base units."""
    return float(value_str) * MULTIPLIERS.get(prefix, 1)


def parse_timestamp(line):
    """Try to parse a timestamp from a line."""
    line = line.strip()
    m = TIMESTAMP_LINE_RE.match(line)
    if m:
        _, month_str, day, time_str, ampm, year = m.groups()
        dt_str = f"{month_str} {day} {time_str} {ampm} {year}"
        try:
            return datetime.strptime(dt_str, "%b %d %I:%M:%S %p %Y")
        except ValueError:
            pass
    return None


def parse_single_iperf_output(text):
    """Parse one iperf client output block and return a result dict."""
    result = {}

    # Client bandwidth (first match = client report)
    bw_matches = CLIENT_BW_RE.findall(text)
    if bw_matches:
        transfer_val, transfer_pfx, bw_val, bw_pfx = bw_matches[0]
        result["client_transfer_bytes"] = parse_size(transfer_val, transfer_pfx)
        result["client_bandwidth_bps"] = parse_size(bw_val, bw_pfx)
        result["client_bandwidth_mbps"] = parse_size(bw_val, bw_pfx) / 1e6

    # Sent datagrams
    sent_m = SENT_RE.search(text)
    if sent_m:
        result["sent_datagrams"] = int(sent_m.group(1))

    # Server report (comes after "Server Report:" line, second bandwidth match)
    sr_m = SERVER_REPORT_RE.search(text)
    if sr_m:
        srv_transfer_val, srv_transfer_pfx = sr_m.group(1), sr_m.group(2)
        srv_bw_val, srv_bw_pfx = sr_m.group(3), sr_m.group(4)
        jitter = sr_m.group(5)
        lost = sr_m.group(6)
        total = sr_m.group(7)
        loss_pct = sr_m.group(8)

        result["server_transfer_bytes"] = parse_size(srv_transfer_val, srv_transfer_pfx)
        result["server_bandwidth_bps"] = parse_size(srv_bw_val, srv_bw_pfx)
        result["server_bandwidth_mbps"] = parse_size(srv_bw_val, srv_bw_pfx) / 1e6
        result["jitter_ms"] = float(jitter)
        result["lost_datagrams"] = int(lost)
        result["total_datagrams"] = int(total)
        result["loss_percent"] = float(loss_pct)

    # Timestamps (first and last date lines)
    timestamps = []
    for line in text.splitlines():
        ts = parse_timestamp(line)
        if ts:
            timestamps.append(ts)
    if timestamps:
        result["start_time"] = timestamps[0].strftime("%Y-%m-%d %H:%M:%S")
        if len(timestamps) > 1:
            result["end_time"] = timestamps[-1].strftime("%Y-%m-%d %H:%M:%S")

    return result


def detect_multi_flow(test_dir):
    """Detect if test directory contains multi-flow outputs.

    Multi-flow files match: client_iperf_RATE_SUBRATE_FLOWID.out
    """
    pattern = re.compile(r'^client_iperf_(\d+)_(\d+)_(\d+)\.out$')
    for f in test_dir.iterdir():
        if pattern.match(f.name):
            return True
    return False


def get_rates_from_files(test_dir, multi_flow):
    """Discover test rates from client_iperf_*.out filenames."""
    rates = set()
    if multi_flow:
        pattern = re.compile(r'^client_iperf_(\d+)_(\d+)_(\d+)\.out$')
        for f in test_dir.iterdir():
            m = pattern.match(f.name)
            if m:
                rates.add(int(m.group(1)))
    else:
        pattern = re.compile(r'^client_iperf_(\d+)\.out$')
        for f in test_dir.iterdir():
            m = pattern.match(f.name)
            if m:
                rates.add(int(m.group(1)))
    return sorted(rates)


def parse_single_flow_test(test_dir, rates):
    """Parse a single-flow test directory."""
    results = []
    for rate in rates:
        path = test_dir / f"client_iperf_{rate}.out"
        if not path.exists():
            continue
        text = path.read_text()
        entry = parse_single_iperf_output(text)
        entry["target_kpps"] = rate
        # Calculate achieved kpps from sent datagrams and test duration
        if "sent_datagrams" in entry:
            # Approximate: iperf reports interval, use 10s default
            entry["achieved_client_kpps"] = round(entry["sent_datagrams"] / 10 / 1000, 1)
        if "total_datagrams" in entry and "lost_datagrams" in entry:
            received = entry["total_datagrams"] - entry["lost_datagrams"]
            entry["achieved_server_kpps"] = round(received / 10 / 1000, 1)
        results.append(entry)
    return results


def parse_multi_flow_test(test_dir, rates):
    """Parse a multi-flow test directory."""
    results = []
    flow_file_re = re.compile(r'^client_iperf_(\d+)_(\d+)_(\d+)\.out$')

    # Discover flow count from files
    flow_ids = set()
    for f in test_dir.iterdir():
        m = flow_file_re.match(f.name)
        if m:
            flow_ids.add(int(m.group(3)))
    flow_count = len(flow_ids) if flow_ids else 1

    for rate in rates:
        entry = {
            "target_kpps": rate,
            "flow_count": flow_count,
            "per_flow": [],
        }

        # Parse wrapper file for timestamps
        wrapper = test_dir / f"client_iperf_{rate}.out"
        if wrapper.exists():
            wrapper_text = wrapper.read_text()
            timestamps = []
            for line in wrapper_text.splitlines():
                ts = parse_timestamp(line)
                if ts:
                    timestamps.append(ts)
            if timestamps:
                entry["start_time"] = timestamps[0].strftime("%Y-%m-%d %H:%M:%S")
                if len(timestamps) > 1:
                    entry["end_time"] = timestamps[-1].strftime("%Y-%m-%d %H:%M:%S")

        # Parse per-flow files
        subrate = rate // flow_count
        agg_sent = 0
        agg_server_received = 0
        agg_lost = 0
        agg_total = 0
        agg_client_bw = 0.0
        agg_server_bw = 0.0

        for fid in sorted(flow_ids):
            flow_path = test_dir / f"client_iperf_{rate}_{subrate}_{fid}.out"
            if not flow_path.exists():
                continue
            text = flow_path.read_text()
            flow_result = parse_single_iperf_output(text)
            flow_result["flow_id"] = fid
            flow_result["target_kpps"] = subrate
            entry["per_flow"].append(flow_result)

            agg_sent += flow_result.get("sent_datagrams", 0)
            agg_lost += flow_result.get("lost_datagrams", 0)
            agg_total += flow_result.get("total_datagrams", 0)
            agg_client_bw += flow_result.get("client_bandwidth_mbps", 0)
            agg_server_bw += flow_result.get("server_bandwidth_mbps", 0)

        agg_server_received = agg_total - agg_lost

        entry["aggregate"] = {
            "sent_datagrams": agg_sent,
            "lost_datagrams": agg_lost,
            "total_datagrams": agg_total,
            "received_datagrams": agg_server_received,
            "client_bandwidth_mbps": round(agg_client_bw, 1),
            "server_bandwidth_mbps": round(agg_server_bw, 1),
            "loss_percent": round(agg_lost / agg_total * 100, 2) if agg_total > 0 else 0,
            "achieved_client_kpps": round(agg_sent / 10 / 1000, 1),
            "achieved_server_kpps": round(agg_server_received / 10 / 1000, 1),
        }

        results.append(entry)

    return results


def format_table(results, multi_flow):
    """Format results as a human-readable table."""
    lines = []
    if multi_flow:
        header = f"{'Rate':>6} {'Flows':>5} {'Sent kpps':>10} {'Recv kpps':>10} {'Client Mbps':>12} {'Server Mbps':>12} {'Loss%':>7}"
        lines.append(header)
        lines.append("-" * len(header))
        for r in results:
            agg = r.get("aggregate", {})
            lines.append(
                f"{r['target_kpps']:>6} {r.get('flow_count', '?'):>5} "
                f"{agg.get('achieved_client_kpps', '?'):>10} "
                f"{agg.get('achieved_server_kpps', '?'):>10} "
                f"{agg.get('client_bandwidth_mbps', '?'):>12} "
                f"{agg.get('server_bandwidth_mbps', '?'):>12} "
                f"{agg.get('loss_percent', '?'):>7}"
            )
    else:
        header = f"{'Rate':>6} {'Sent kpps':>10} {'Recv kpps':>10} {'Client Mbps':>12} {'Server Mbps':>12} {'Loss%':>7} {'Jitter ms':>10}"
        lines.append(header)
        lines.append("-" * len(header))
        for r in results:
            lines.append(
                f"{r.get('target_kpps', '?'):>6} "
                f"{r.get('achieved_client_kpps', '?'):>10} "
                f"{r.get('achieved_server_kpps', '?'):>10} "
                f"{r.get('client_bandwidth_mbps', '?'):>12} "
                f"{r.get('server_bandwidth_mbps', '?'):>12} "
                f"{r.get('loss_percent', '?'):>7} "
                f"{r.get('jitter_ms', '?'):>10}"
            )
    return "\n".join(lines)


def main():
    if len(sys.argv) < 2:
        print("Usage: parse_iperf.py <test-directory> [--pretty] [--output-format json|table]",
              file=sys.stderr)
        sys.exit(1)

    test_dir = Path(sys.argv[1])
    pretty = "--pretty" in sys.argv
    output_format = "json"
    if "--output-format" in sys.argv:
        idx = sys.argv.index("--output-format")
        if idx + 1 < len(sys.argv):
            output_format = sys.argv[idx + 1]

    if not test_dir.is_dir():
        print(f"Error: {test_dir} is not a directory", file=sys.stderr)
        sys.exit(1)

    multi_flow = detect_multi_flow(test_dir)
    rates = get_rates_from_files(test_dir, multi_flow)

    if not rates:
        print("Error: No client_iperf_*.out files found", file=sys.stderr)
        sys.exit(1)

    if multi_flow:
        results = parse_multi_flow_test(test_dir, rates)
    else:
        results = parse_single_flow_test(test_dir, rates)

    output = {
        "test_directory": str(test_dir.resolve()),
        "multi_flow": multi_flow,
        "rates_tested": rates,
        "results": results,
    }

    if output_format == "table":
        print(format_table(results, multi_flow))
    elif pretty:
        print(json.dumps(output, indent=2))
    else:
        print(json.dumps(output))


if __name__ == "__main__":
    main()
