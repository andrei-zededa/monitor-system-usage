package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/andrei-zededa/monitor-system-usage/pkg/msuformat"
)

type netDevStats struct {
	RxBytes   uint64
	RxPackets uint64
	RxErrs    uint64
	RxDrop    uint64
	TxBytes   uint64
	TxPackets uint64
	TxErrs    uint64
	TxDrop    uint64
}

// parseNetDev parses /proc/net/dev output lines into per-interface counters.
func parseNetDev(lines []string) map[string]netDevStats {
	result := make(map[string]netDevStats)
	for _, line := range lines {
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:idx])
		if iface == "" {
			continue
		}
		rest := strings.TrimSpace(line[idx+1:])
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			continue
		}
		var s netDevStats
		s.RxBytes, _ = strconv.ParseUint(fields[0], 10, 64)
		s.RxPackets, _ = strconv.ParseUint(fields[1], 10, 64)
		s.RxErrs, _ = strconv.ParseUint(fields[2], 10, 64)
		s.RxDrop, _ = strconv.ParseUint(fields[3], 10, 64)
		s.TxBytes, _ = strconv.ParseUint(fields[8], 10, 64)
		s.TxPackets, _ = strconv.ParseUint(fields[9], 10, 64)
		s.TxErrs, _ = strconv.ParseUint(fields[10], 10, 64)
		s.TxDrop, _ = strconv.ParseUint(fields[11], 10, 64)
		result[iface] = s
	}
	return result
}

// processNetDevSnapshots computes per-interface rates from consecutive
// /proc/net/dev snapshots.
func processNetDevSnapshots(snapshots []msuSnapshot) map[string][]cpuDataPoint {
	series := make(map[string][]cpuDataPoint)
	ifaceHasTraffic := make(map[string]bool)

	for i := 1; i < len(snapshots); i++ {
		dt := snapshots[i].time.Sub(snapshots[i-1].time).Seconds()
		if dt <= 0 {
			continue
		}
		prev := parseNetDev(snapshots[i-1].lines)
		curr := parseNetDev(snapshots[i].lines)
		t := snapshots[i].time

		for iface, cs := range curr {
			if iface == "lo" {
				continue
			}
			ps, ok := prev[iface]
			if !ok {
				continue
			}

			rxPktDelta := safeU64Delta(cs.RxPackets, ps.RxPackets)
			txPktDelta := safeU64Delta(cs.TxPackets, ps.TxPackets)
			rxByteDelta := safeU64Delta(cs.RxBytes, ps.RxBytes)
			txByteDelta := safeU64Delta(cs.TxBytes, ps.TxBytes)
			rxDropDelta := safeU64Delta(cs.RxDrop, ps.RxDrop)
			txDropDelta := safeU64Delta(cs.TxDrop, ps.TxDrop)
			rxErrDelta := safeU64Delta(cs.RxErrs, ps.RxErrs)
			txErrDelta := safeU64Delta(cs.TxErrs, ps.TxErrs)

			if rxPktDelta > 0 || txPktDelta > 0 {
				ifaceHasTraffic[iface] = true
			}

			appendRate := func(key string, delta uint64) {
				series[key] = append(series[key], cpuDataPoint{Time: t, Value: float64(delta) / dt})
			}

			appendRate(fmt.Sprintf("%s rx_pps", iface), rxPktDelta)
			appendRate(fmt.Sprintf("%s tx_pps", iface), txPktDelta)
			series[fmt.Sprintf("%s rx_Mbps", iface)] = append(series[fmt.Sprintf("%s rx_Mbps", iface)],
				cpuDataPoint{Time: t, Value: float64(rxByteDelta) * 8 / 1e6 / dt})
			series[fmt.Sprintf("%s tx_Mbps", iface)] = append(series[fmt.Sprintf("%s tx_Mbps", iface)],
				cpuDataPoint{Time: t, Value: float64(txByteDelta) * 8 / 1e6 / dt})

			// Always include drop/error rates (even zero) so the chart baseline is visible
			appendRate(fmt.Sprintf("%s rx_drop/s", iface), rxDropDelta)
			appendRate(fmt.Sprintf("%s tx_drop/s", iface), txDropDelta)
			appendRate(fmt.Sprintf("%s rx_err/s", iface), rxErrDelta)
			appendRate(fmt.Sprintf("%s tx_err/s", iface), txErrDelta)
		}
	}

	// Remove interfaces with no traffic
	for key := range series {
		iface := strings.Fields(key)[0]
		if !ifaceHasTraffic[iface] {
			delete(series, key)
		}
	}

	return series
}

// --- sysfs /sys/class/net/*/statistics/* processing ---

// sysfsStatRe matches sysfs drop/error commands like "cat /sys/class/net/nbu3x1/statistics/tx_dropped".
// Only drops and errors — packets/bytes are already covered by /proc/net/dev.
var sysfsStatRe = regexp.MustCompile(`^cat /sys/class/net/([^/]+)/statistics/((?:rx|tx)_(?:dropped|errors))$`)

// discoverSysfsStatCommands returns a set of command strings for all sysfs
// statistics counters found in the msu data, by scanning command names.
// It only selects rx/tx packets, bytes, dropped, and errors.
func discoverSysfsStatCommands(filename string) map[string]bool {
	// We scan the file for command markers matching the sysfs pattern.
	// For legacy text files, these are lines like "---> cat /sys/class/net/..."
	// For CBOR, we read sample Cmd fields.
	commands := make(map[string]bool)

	if isCBORFile(filename) {
		f, err := os.Open(filename)
		if err != nil {
			return commands
		}
		defer f.Close()
		r := msuformat.NewReader(f)
		r.ReadHeader()
		seen := make(map[string]bool)
		for {
			s, err := r.Next()
			if err != nil || s == nil {
				break
			}
			if seen[s.Cmd] {
				continue
			}
			seen[s.Cmd] = true
			if sysfsStatRe.MatchString(s.Cmd) {
				commands[s.Cmd] = true
			}
		}
	} else {
		f, err := os.Open(filename)
		if err != nil {
			return commands
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		cmdMarkerRe := regexp.MustCompile(`^-+>\s*(.+)$`)
		seen := make(map[string]bool)
		for scanner.Scan() {
			line := scanner.Text()
			m := cmdMarkerRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			cmd := strings.TrimSpace(m[1])
			if seen[cmd] {
				continue
			}
			seen[cmd] = true
			if sysfsStatRe.MatchString(cmd) {
				commands[cmd] = true
			}
		}
	}
	return commands
}

// processSysfsStatSnapshots processes sysfs statistics snapshots (each snapshot
// is a single-line number) and produces rate series keyed like "nbu3x1 sysfs tx_dropped/s".
func processSysfsStatSnapshots(allSnaps map[string][]msuSnapshot) map[string][]cpuDataPoint {
	series := make(map[string][]cpuDataPoint)
	ifaceHasActivity := make(map[string]bool)

	for cmd, snapshots := range allSnaps {
		m := sysfsStatRe.FindStringSubmatch(cmd)
		if m == nil || len(snapshots) < 2 {
			continue
		}
		iface := m[1]
		counter := m[2] // e.g. "tx_dropped", "rx_packets"

		for i := 1; i < len(snapshots); i++ {
			dt := snapshots[i].time.Sub(snapshots[i-1].time).Seconds()
			if dt <= 0 {
				continue
			}
			prevVal := parseSingleUint64(snapshots[i-1].lines)
			currVal := parseSingleUint64(snapshots[i].lines)
			delta := safeU64Delta(currVal, prevVal)
			rate := float64(delta) / dt

			key := fmt.Sprintf("%s sysfs %s/s", iface, counter)
			series[key] = append(series[key], cpuDataPoint{Time: snapshots[i].time, Value: rate})

			if delta > 0 {
				ifaceHasActivity[iface+"/"+counter] = true
			}
		}
	}

	// Only keep series that had non-zero deltas at some point
	// (to avoid charting completely flat zero counters)
	for key := range series {
		parts := strings.Fields(key)
		if len(parts) < 3 {
			delete(series, key)
			continue
		}
		iface := parts[0]
		counterWithSlash := parts[2] // e.g. "tx_dropped/s"
		counter := strings.TrimSuffix(counterWithSlash, "/s")
		if !ifaceHasActivity[iface+"/"+counter] {
			delete(series, key)
		}
	}

	return series
}

func parseSingleUint64(lines []string) uint64 {
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err == nil {
			return v
		}
	}
	return 0
}

func safeU64Delta(curr, prev uint64) uint64 {
	if curr >= prev {
		return curr - prev
	}
	return 0
}
