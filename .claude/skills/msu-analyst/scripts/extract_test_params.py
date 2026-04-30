#!/usr/bin/env python3
"""
Extract test parameters from a run.sh test script.

Parses variable assignments, IP configuration, iperf invocations, and core
pinning to produce a structured JSON description of the test topology.

Usage:
    extract_test_params.py <test-directory>
    extract_test_params.py <test-directory> --pretty
"""

import json
import re
import sys
from pathlib import Path


def parse_run_sh(text):
    """Parse run.sh content and return a dict of test parameters."""
    params = {}

    # --- Shell variable assignments (CNS=, CIF=, CCORE=, etc.) ---
    var_re = re.compile(r'^(\w+)="([^"]+)"', re.MULTILINE)
    shell_vars = {}
    for m in var_re.finditer(text):
        shell_vars[m.group(1)] = m.group(2)

    params["client_namespace"] = shell_vars.get("CNS", "")
    params["client_interface"] = shell_vars.get("CIF", "")
    params["client_core_base"] = int(shell_vars.get("CCORE", "0"))
    params["server_namespace"] = shell_vars.get("SNS", "")
    params["server_interface"] = shell_vars.get("SIF", "")
    params["server_core_base"] = int(shell_vars.get("SCORE", "0"))

    # --- Test duration ---
    tlen_m = re.search(r'^TLEN="(\d+)"', text, re.MULTILINE)
    params["test_duration_sec"] = int(tlen_m.group(1)) if tlen_m else 10

    # --- Datagram size (from -l flag in iperf client command) ---
    dgram_m = re.search(r'iperf\s+.*-l\s+(\d+)', text)
    params["datagram_size_bytes"] = int(dgram_m.group(1)) if dgram_m else 100

    # --- Test rates from 'for CRATE in ...' loop ---
    rate_m = re.search(r'for\s+CRATE\s+in\s+([^;]+);', text)
    if rate_m:
        rates = [int(r) for r in rate_m.group(1).split()]
        params["test_rates_kpps"] = rates
    else:
        params["test_rates_kpps"] = []

    # --- Client IP addresses ---
    client_ips = []
    cif = re.escape(shell_vars.get("CIF", ""))
    # Match: ip addr add IP/MASK dev $CIF  or  ip addr add IP/MASK dev "$CIF"
    # Also handle indirect: dev enp4s0f0, dev "$CIF", or dev ${CIF}
    for m in re.finditer(
        r'ip\s+addr\s+add\s+(\d+\.\d+\.\d+\.\d+)/\d+\s+dev\s+["\${}]*'
        + cif, text
    ):
        client_ips.append(m.group(1))
    # Fallback: look for the variable-expanded form
    if not client_ips:
        for m in re.finditer(
            r'ip\s+addr\s+add\s+(\d+\.\d+\.\d+\.\d+)/\d+\s+dev\s+"\$CIF"',
            text,
        ):
            client_ips.append(m.group(1))
    params["client_ips"] = client_ips

    # --- Server IP addresses ---
    server_ips = []
    sif = re.escape(shell_vars.get("SIF", ""))
    for m in re.finditer(
        r'ip\s+addr\s+add\s+(\d+\.\d+\.\d+\.\d+)/\d+\s+dev\s+["\${}]*'
        + sif, text
    ):
        server_ips.append(m.group(1))
    if not server_ips:
        for m in re.finditer(
            r'ip\s+addr\s+add\s+(\d+\.\d+\.\d+\.\d+)/\d+\s+dev\s+"\$SIF"',
            text,
        ):
            server_ips.append(m.group(1))
    params["server_ips"] = server_ips

    # --- Multi-flow detection ---
    # Look for subrate calculation: subrate="$(( CRATE / N ))"
    subrate_m = re.search(r'subrate="\$\(\(\s*CRATE\s*/\s*(\d+)\s*\)\)"', text)
    if subrate_m:
        flow_count = int(subrate_m.group(1))
    else:
        flow_count = max(len(client_ips), 1)
    params["flow_count"] = flow_count
    params["multi_flow"] = flow_count > 1

    # --- Server cores (from iperf server launch lines) ---
    server_cores = []
    # Direct: taskset -c "SCORE" or taskset -c "$SCORE"
    for m in re.finditer(
        r'iperf\s+-s\s+-B\s+(\d+\.\d+\.\d+\.\d+).*?taskset\s+-c\s+"?([^"]+)"?'
        + r'|taskset\s+-c\s+"?([^"]+)"?\s+iperf\s+-s\s+-B\s+(\d+\.\d+\.\d+\.\d+)',
        text,
    ):
        pass  # Complex; use a simpler approach below

    # Parse server taskset lines more directly
    server_cores = []
    for line in text.splitlines():
        if "iperf -s -B" in line and "taskset -c" in line:
            # Extract the core expression
            tc_m = re.search(r'taskset\s+-c\s+"?([^"]*?)"?\s+iperf', line)
            if tc_m:
                core_expr = tc_m.group(1)
                # Resolve shell arithmetic: $(( SCORE + N )) or $SCORE or literal
                arith_m = re.search(r'\$\(\(\s*(?:SCORE|\w+)\s*\+\s*(\d+)\s*\)\)', core_expr)
                if arith_m:
                    offset = int(arith_m.group(1))
                    server_cores.append(params["server_core_base"] + offset)
                elif core_expr.startswith("$"):
                    server_cores.append(params["server_core_base"])
                else:
                    try:
                        server_cores.append(int(core_expr))
                    except ValueError:
                        server_cores.append(params["server_core_base"])

    params["server_cores"] = server_cores if server_cores else [params["server_core_base"]]

    # --- Client cores ---
    client_cores = []
    if params["multi_flow"]:
        for i in range(flow_count):
            client_cores.append(params["client_core_base"] + i)
    else:
        client_cores.append(params["client_core_base"])
    params["client_cores"] = client_cores

    return params


def main():
    if len(sys.argv) < 2:
        print("Usage: extract_test_params.py <test-directory> [--pretty]", file=sys.stderr)
        sys.exit(1)

    test_dir = Path(sys.argv[1])
    pretty = "--pretty" in sys.argv

    run_sh = test_dir / "run.sh"
    if not run_sh.exists():
        print(f"Error: {run_sh} not found", file=sys.stderr)
        sys.exit(1)

    text = run_sh.read_text()
    params = parse_run_sh(text)
    params["test_directory"] = str(test_dir.resolve())
    params["test_name"] = test_dir.name

    if pretty:
        print(json.dumps(params, indent=2))
    else:
        print(json.dumps(params))


if __name__ == "__main__":
    main()
