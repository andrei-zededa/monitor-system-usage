package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andrei-zededa/monitor-system-usage/pkg/msuformat"
)

type cpuDataPoint struct {
	Time  time.Time
	Value float64
}

type cpuDataSeries struct {
	Name   string
	Points []cpuDataPoint
}

type cpuSystemData struct {
	Name   string
	Series []cpuDataSeries
}

type systemFiles struct {
	msuOut          string
	statusCPU10s    string
	statusCPU60s    string
	timeSeriesUsage string
	timeSeriesTotal string
}

func discoverCPUCompareFiles(dir string) (map[string]*systemFiles, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	systems := make(map[string]*systemFiles)
	ensure := func(name string) *systemFiles {
		if _, ok := systems[name]; !ok {
			systems[name] = &systemFiles{}
		}
		return systems[name]
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		p := filepath.Join(dir, n)

		switch {
		case strings.HasSuffix(n, ".msu.cbor"):
			sys := strings.TrimSuffix(n, ".msu.cbor")
			ensure(sys).msuOut = p
		case strings.HasSuffix(n, "_monitor_system_usage.out"):
			sys := strings.TrimSuffix(n, "_monitor_system_usage.out")
			ensure(sys).msuOut = p
		case strings.Contains(n, ".status.cpu_util.10s"):
			sys := n[:strings.Index(n, ".status.cpu_util.")]
			ensure(sys).statusCPU10s = p
		case strings.Contains(n, ".status.cpu_util.60s"):
			sys := n[:strings.Index(n, ".status.cpu_util.")]
			ensure(sys).statusCPU60s = p
		case strings.Contains(n, ".timeSeries.CPU_USAGE."):
			sys := n[:strings.Index(n, ".timeSeries.")]
			ensure(sys).timeSeriesUsage = p
		case strings.Contains(n, ".timeSeries.CPU_TOTAL."):
			sys := n[:strings.Index(n, ".timeSeries.")]
			ensure(sys).timeSeriesTotal = p
		}
	}

	return systems, nil
}

// --- MSU file scanning ---

type msuSnapshot struct {
	time  time.Time
	lines []string
}

// collectMSUSnapshots scans an msu.out file once and collects output
// snapshots for every command in the given set.
func collectMSUSnapshots(filename string, commands map[string]bool) (map[string][]msuSnapshot, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	aBeginRe := regexp.MustCompile(`^\+\+\+\+ BEGIN\s+(\d+)\s+(\S+)`)
	aEndRe := regexp.MustCompile(`^\+\+\+\+ END\s+(\d+)`)
	bBeginRe := regexp.MustCompile(`^==== BEGIN\s+(\d+)\s+(\S+)`)
	bEndRe := regexp.MustCompile(`^==== END\s+(\d+)`)
	cmdMarkerRe := regexp.MustCompile(`^-+>\s*(.+)$`)

	result := make(map[string][]msuSnapshot)
	var currentTime time.Time
	inSection := false
	var activeCmd string
	var cmdLines []string

	flush := func() {
		if activeCmd != "" && len(cmdLines) > 0 {
			result[activeCmd] = append(result[activeCmd], msuSnapshot{time: currentTime, lines: cmdLines})
		}
		cmdLines = nil
		activeCmd = ""
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		if m := aBeginRe.FindStringSubmatch(line); m != nil {
			flush()
			if ts, err := time.Parse(timestampFmt, m[2]); err == nil {
				currentTime = ts
				inSection = true
			}
			continue
		}
		if aEndRe.MatchString(line) {
			flush()
			inSection = false
			continue
		}
		if m := bBeginRe.FindStringSubmatch(line); m != nil {
			flush()
			if ts, err := time.Parse(timestampFmt, m[2]); err == nil {
				currentTime = ts
				inSection = true
			}
			continue
		}
		if bEndRe.MatchString(line) {
			flush()
			inSection = false
			continue
		}

		if !inSection {
			continue
		}

		if m := cmdMarkerRe.FindStringSubmatch(line); m != nil {
			flush()
			cmdName := strings.TrimSpace(m[1])
			if commands[cmdName] {
				activeCmd = cmdName
				cmdLines = []string{}
			}
			continue
		}

		if activeCmd != "" {
			cmdLines = append(cmdLines, line)
		}
	}
	flush()

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

// collectMSUSnapshotsCBOR reads an MSU CBOR file and collects snapshots
// for the requested commands, returning the same type as collectMSUSnapshots.
func collectMSUSnapshotsCBOR(filename string, commands map[string]bool) (map[string][]msuSnapshot, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := msuformat.NewReader(f)

	// Read and discard the header.
	if _, err := r.ReadHeader(); err != nil {
		return nil, fmt.Errorf("reading CBOR header: %w", err)
	}

	result := make(map[string][]msuSnapshot)
	for {
		sample, err := r.Next()
		if err != nil {
			return nil, fmt.Errorf("reading CBOR sample: %w", err)
		}
		if sample == nil {
			break
		}
		if !commands[sample.Cmd] {
			continue
		}
		ts, err := sample.ParseTime()
		if err != nil {
			continue
		}
		lines := strings.Split(sample.Out, "\n")
		result[sample.Cmd] = append(result[sample.Cmd], msuSnapshot{
			time:  ts,
			lines: lines,
		})
	}

	return result, nil
}

// isCBORFile peeks at the first byte to determine if the file is CBOR.
// CBOR maps start with 0xa0-0xbf (map) or 0xd8-0xdb (tag).
func isCBORFile(filename string) bool {
	f, err := os.Open(filename)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [1]byte
	n, err := f.Read(buf[:])
	if n == 0 || err != nil {
		return false
	}
	// CBOR major type 5 (map) starts at 0xa0; also check for tags (0xc0+).
	return buf[0] >= 0xa0
}

// collectMSUSnapshotsAuto detects the file format and dispatches to the
// appropriate reader (legacy text or CBOR).
func collectMSUSnapshotsAuto(filename string, commands map[string]bool) (map[string][]msuSnapshot, error) {
	if isCBORFile(filename) {
		return collectMSUSnapshotsCBOR(filename, commands)
	}
	return collectMSUSnapshots(filename, commands)
}

// --- /proc/stat processing ---

var procStatFields = []string{
	"user", "nice", "system", "idle",
	"iowait", "irq", "softirq", "steal",
	"guest", "guest_nice",
}

const softirqFieldIdx = 6

// computeCPUFieldPcts computes per-field percentages from the aggregate
// "cpu" line plus per-CPU softirq percentages.
func computeCPUFieldPcts(prevLines, currLines []string) (fields map[string]float64, perCPUSoftirq map[string]float64, util, utilNoIOW float64, ok bool) {
	prevCPU := parseCPUStats(prevLines)
	currCPU := parseCPUStats(currLines)

	// Aggregate "cpu" line
	prevVals, ok1 := prevCPU["cpu"]
	currVals, ok2 := currCPU["cpu"]
	if !ok1 || !ok2 {
		return nil, nil, 0, 0, false
	}
	n := len(currVals)
	if len(prevVals) < n {
		n = len(prevVals)
	}
	if n < 5 {
		return nil, nil, 0, 0, false
	}

	deltas := make([]uint64, n)
	var totalDelta uint64
	for i := 0; i < n; i++ {
		if currVals[i] >= prevVals[i] {
			deltas[i] = currVals[i] - prevVals[i]
		}
		totalDelta += deltas[i]
	}
	if totalDelta == 0 {
		return nil, nil, 0, 0, false
	}

	fields = make(map[string]float64, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("field%d", i)
		if i < len(procStatFields) {
			name = procStatFields[i]
		}
		fields[name] = float64(deltas[i]) / float64(totalDelta) * 100.0
	}

	idlePct := float64(deltas[3]) / float64(totalDelta) * 100.0
	idleAndIowPct := float64(deltas[3]+deltas[4]) / float64(totalDelta) * 100.0

	// Per-CPU softirq %
	perCPUSoftirq = make(map[string]float64)
	for cpuName, prev := range prevCPU {
		if cpuName == "cpu" {
			continue
		}
		curr, exists := currCPU[cpuName]
		if !exists {
			continue
		}
		cn := len(curr)
		if len(prev) < cn {
			cn = len(prev)
		}
		if cn <= softirqFieldIdx {
			continue
		}
		var cpuTotal uint64
		for i := 0; i < cn; i++ {
			if curr[i] >= prev[i] {
				cpuTotal += curr[i] - prev[i]
			}
		}
		if cpuTotal == 0 {
			continue
		}
		var sirqDelta uint64
		if curr[softirqFieldIdx] >= prev[softirqFieldIdx] {
			sirqDelta = curr[softirqFieldIdx] - prev[softirqFieldIdx]
		}
		perCPUSoftirq[cpuName] = float64(sirqDelta) / float64(cpuTotal) * 100.0
	}

	return fields, perCPUSoftirq, 100.0 - idlePct, 100.0 - idleAndIowPct, true
}

func processProcStatSnapshots(snapshots []msuSnapshot) map[string][]cpuDataPoint {
	series := make(map[string][]cpuDataPoint)

	for i := 1; i < len(snapshots); i++ {
		fields, perCPUSoftirq, util, utilNoIOW, ok := computeCPUFieldPcts(snapshots[i-1].lines, snapshots[i].lines)
		if !ok {
			continue
		}
		t := snapshots[i].time
		series["/proc/stat (iowait=busy)"] = append(series["/proc/stat (iowait=busy)"], cpuDataPoint{Time: t, Value: util})
		series["/proc/stat (iowait=idle)"] = append(series["/proc/stat (iowait=idle)"], cpuDataPoint{Time: t, Value: utilNoIOW})
		for name, pct := range fields {
			if name == "idle" {
				continue
			}
			key := "/proc/stat " + name
			series[key] = append(series[key], cpuDataPoint{Time: t, Value: pct})
		}
		for cpuName, pct := range perCPUSoftirq {
			key := fmt.Sprintf("%s softirq %%", cpuName)
			series[key] = append(series[key], cpuDataPoint{Time: t, Value: pct})
		}
	}

	return series
}

// --- /proc/softirqs processing ---

// parseSoftirqsTable parses /proc/softirqs output into map[type][]uint64.
func parseSoftirqsTable(lines []string) (map[string][]uint64, int) {
	if len(lines) < 2 {
		return nil, 0
	}
	// First line is the CPU header; count CPUs
	headerFields := strings.Fields(lines[0])
	numCPUs := len(headerFields)

	result := make(map[string][]uint64)
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		typeName := strings.TrimSuffix(fields[0], ":")
		vals := make([]uint64, numCPUs)
		for i := 0; i < numCPUs && i+1 < len(fields); i++ {
			vals[i], _ = strconv.ParseUint(fields[i+1], 10, 64)
		}
		result[typeName] = vals
	}
	return result, numCPUs
}

func processSoftirqSnapshots(snapshots []msuSnapshot) map[string][]cpuDataPoint {
	series := make(map[string][]cpuDataPoint)

	for i := 1; i < len(snapshots); i++ {
		dt := snapshots[i].time.Sub(snapshots[i-1].time).Seconds()
		if dt <= 0 {
			continue
		}
		prev, numCPUs := parseSoftirqsTable(snapshots[i-1].lines)
		curr, _ := parseSoftirqsTable(snapshots[i].lines)
		if prev == nil || curr == nil {
			continue
		}

		t := snapshots[i].time

		for _, typeName := range []string{"NET_RX", "NET_TX"} {
			pv, ok1 := prev[typeName]
			cv, ok2 := curr[typeName]
			if !ok1 || !ok2 {
				continue
			}
			var totalRate float64
			for cpu := 0; cpu < numCPUs && cpu < len(pv) && cpu < len(cv); cpu++ {
				if cv[cpu] >= pv[cpu] {
					rate := float64(cv[cpu]-pv[cpu]) / dt
					totalRate += rate
					key := fmt.Sprintf("softirqs %s cpu%d /s", typeName, cpu)
					series[key] = append(series[key], cpuDataPoint{Time: t, Value: rate})
				}
			}
			key := fmt.Sprintf("softirqs %s total /s", typeName)
			series[key] = append(series[key], cpuDataPoint{Time: t, Value: totalRate})
		}
	}

	return series
}

// --- /proc/net/softnet_stat processing ---

func parseSoftnetStat(lines []string) [][]uint64 {
	var result [][]uint64
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		vals := make([]uint64, len(fields))
		for i, f := range fields {
			vals[i], _ = strconv.ParseUint(f, 16, 64)
		}
		result = append(result, vals)
	}
	return result
}

// softnet_stat field indices
const (
	softnetProcessed = 0
	softnetDropped   = 1
	softnetSqueeze   = 2
)

func processSoftnetStatSnapshots(snapshots []msuSnapshot) map[string][]cpuDataPoint {
	series := make(map[string][]cpuDataPoint)

	for i := 1; i < len(snapshots); i++ {
		dt := snapshots[i].time.Sub(snapshots[i-1].time).Seconds()
		if dt <= 0 {
			continue
		}
		prev := parseSoftnetStat(snapshots[i-1].lines)
		curr := parseSoftnetStat(snapshots[i].lines)
		if len(prev) == 0 || len(curr) == 0 {
			continue
		}

		t := snapshots[i].time
		numCPUs := len(curr)
		if len(prev) < numCPUs {
			numCPUs = len(prev)
		}

		type metric struct {
			idx  int
			name string
		}
		metrics := []metric{
			{softnetProcessed, "processed"},
			{softnetDropped, "drops"},
			{softnetSqueeze, "squeeze"},
		}

		for _, m := range metrics {
			var totalRate float64
			for cpu := 0; cpu < numCPUs; cpu++ {
				if m.idx >= len(curr[cpu]) || m.idx >= len(prev[cpu]) {
					continue
				}
				if curr[cpu][m.idx] >= prev[cpu][m.idx] {
					rate := float64(curr[cpu][m.idx]-prev[cpu][m.idx]) / dt
					totalRate += rate
					key := fmt.Sprintf("softnet %s cpu%d /s", m.name, cpu)
					series[key] = append(series[key], cpuDataPoint{Time: t, Value: rate})
				}
			}
			key := fmt.Sprintf("softnet %s total /s", m.name)
			series[key] = append(series[key], cpuDataPoint{Time: t, Value: totalRate})
		}
	}

	return series
}

// --- ps auxww processing (QEMU/vhost thread CPU) ---

// parsePsTime parses the TIME column from ps (e.g. "3:53", "0:00", "12:34:56") into seconds.
func parsePsTime(s string) float64 {
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 2:
		m, _ := strconv.ParseFloat(parts[0], 64)
		sec, _ := strconv.ParseFloat(parts[1], 64)
		return m*60 + sec
	case 3:
		h, _ := strconv.ParseFloat(parts[0], 64)
		m, _ := strconv.ParseFloat(parts[1], 64)
		sec, _ := strconv.ParseFloat(parts[2], 64)
		return h*3600 + m*60 + sec
	}
	return 0
}

// psThreadInfo holds the cumulative CPU time for a process/thread from ps output.
type psThreadInfo struct {
	pid     string
	cpuSecs float64
	label   string // e.g. "qemu PID 5756", "vhost-5756 PID 5781"
}

// parseQemuVhostFromPs extracts QEMU processes and their associated vhost/kvm
// kernel threads from ps auxww output lines. Returns a map of label -> cumulative CPU seconds.
func parseQemuVhostFromPs(lines []string) map[string]float64 {
	result := make(map[string]float64)
	// First pass: find QEMU PIDs
	qemuPIDs := make(map[string]bool)
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 11 {
			continue
		}
		cmd := strings.Join(fields[10:], " ")
		if strings.Contains(cmd, "qemu-system") {
			pid := fields[1]
			qemuPIDs[pid] = true
			timeSecs := parsePsTime(fields[9])
			label := fmt.Sprintf("qemu PID %s", pid)
			result[label] = timeSecs
		}
	}
	// Second pass: find vhost/kvm threads associated with QEMU PIDs
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 11 {
			continue
		}
		cmd := strings.Join(fields[10:], " ")
		// Match kernel threads like [vhost-5756], [kvm-pit/5756], [kvm-nx-lpage-recovery-5756]
		if !strings.HasPrefix(cmd, "[") {
			continue
		}
		for qpid := range qemuPIDs {
			if strings.Contains(cmd, qpid) {
				pid := fields[1]
				timeSecs := parsePsTime(fields[9])
				// Extract thread name from [name]
				threadName := strings.Trim(cmd, "[]")
				label := fmt.Sprintf("%s PID %s", threadName, pid)
				result[label] = timeSecs
				break
			}
		}
	}
	return result
}

func processQemuThreadSnapshots(snapshots []msuSnapshot) map[string][]cpuDataPoint {
	series := make(map[string][]cpuDataPoint)

	for i := 1; i < len(snapshots); i++ {
		dt := snapshots[i].time.Sub(snapshots[i-1].time).Seconds()
		if dt <= 0 {
			continue
		}
		prev := parseQemuVhostFromPs(snapshots[i-1].lines)
		curr := parseQemuVhostFromPs(snapshots[i].lines)

		t := snapshots[i].time
		for label, currSecs := range curr {
			prevSecs, ok := prev[label]
			if !ok {
				continue
			}
			delta := currSecs - prevSecs
			if delta < 0 {
				continue
			}
			pct := delta / dt * 100.0
			series[label] = append(series[label], cpuDataPoint{Time: t, Value: pct})
		}
	}

	return series
}

// --- Cloud API file parsers ---

func parseStatusCPUUtil(filename string) ([]cpuDataPoint, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var points []cpuDataPoint
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "\t\t")
		if len(parts) != 2 {
			continue
		}

		dateStr := strings.TrimSpace(parts[0])
		valStr := strings.TrimSpace(parts[1])

		dateStr = strings.Replace(dateStr, " CEST ", " +0200 ", 1)
		dateStr = strings.Replace(dateStr, " CET ", " +0100 ", 1)
		dateStr = strings.Replace(dateStr, " UTC ", " +0000 ", 1)

		t, err := time.Parse("Mon Jan 2 03:04:05 PM -0700 2006", dateStr)
		if err != nil {
			continue
		}

		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}

		points = append(points, cpuDataPoint{Time: t.UTC(), Value: val})
	}

	return points, scanner.Err()
}

type timeSeriesJSON struct {
	List []struct {
		Timestamp string    `json:"timestamp"`
		Values    []float64 `json:"values"`
	} `json:"list"`
}

func parseTimeSeriesJSON(filename string) ([]cpuDataPoint, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(strings.NewReader(string(data)))
	entries := make(map[string]float64)

	for decoder.More() {
		var obj timeSeriesJSON
		if err := decoder.Decode(&obj); err != nil {
			break
		}
		for _, entry := range obj.List {
			if len(entry.Values) > 0 {
				ts := entry.Timestamp
				val := entry.Values[0]
				prev, exists := entries[ts]
				if !exists || (prev == 0 && val != 0) {
					entries[ts] = val
				}
			}
		}
	}

	var points []cpuDataPoint
	for tsStr, val := range entries {
		t, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			continue
		}
		points = append(points, cpuDataPoint{Time: t.UTC(), Value: val})
	}

	sort.Slice(points, func(i, j int) bool {
		return points[i].Time.Before(points[j].Time)
	})

	return points, nil
}

// --- Orchestration ---

// addSeriesOrdered appends named series from a map to sysData in the given key order,
// logging each to stderr.
func addSeriesOrdered(sysData *cpuSystemData, sysName string, seriesMap map[string][]cpuDataPoint, keys []string) {
	for _, key := range keys {
		pts := seriesMap[key]
		if len(pts) > 0 {
			sysData.Series = append(sysData.Series, cpuDataSeries{Name: key, Points: pts})
			fmt.Fprintf(os.Stderr, "  %s / %s: %d data points\n", sysName, key, len(pts))
		}
	}
}

func runCPUCompare(dir string) error {
	fileSets, err := discoverCPUCompareFiles(dir)
	if err != nil {
		return fmt.Errorf("discovering files: %w", err)
	}
	if len(fileSets) == 0 {
		return fmt.Errorf("no CPU data files found in %s", dir)
	}

	var systems []cpuSystemData

	var sysNames []string
	for name := range fileSets {
		sysNames = append(sysNames, name)
	}
	sort.Strings(sysNames)

	for _, sysName := range sysNames {
		fs := fileSets[sysName]
		cpuPctData := cpuSystemData{Name: sysName}
		rateData := cpuSystemData{Name: sysName + " - Softirq & Network Rates"}
		var qemuData cpuSystemData

		if fs.msuOut != "" {
			// Collect all commands in one pass
			snaps, err := collectMSUSnapshotsAuto(fs.msuOut, map[string]bool{
				"cat /proc/stat":             true,
				"cat /proc/softirqs":         true,
				"cat /proc/net/softnet_stat": true,
				"ps auxwww":                  true,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", fs.msuOut, err)
			} else {
				// /proc/stat → CPU % chart
				if statSnaps := snaps["cat /proc/stat"]; len(statSnaps) > 1 {
					statSeries := processProcStatSnapshots(statSnaps)

					// Determine CPU count from series keys
					var cpuNames []string
					for k := range statSeries {
						if strings.HasSuffix(k, " softirq %") {
							cpuNames = append(cpuNames, k)
						}
					}
					sort.Strings(cpuNames)

					orderedKeys := []string{"/proc/stat (iowait=busy)", "/proc/stat (iowait=idle)"}
					for _, f := range procStatFields {
						if f == "idle" {
							continue
						}
						orderedKeys = append(orderedKeys, "/proc/stat "+f)
					}
					orderedKeys = append(orderedKeys, cpuNames...)

					addSeriesOrdered(&cpuPctData, sysName, statSeries, orderedKeys)
				}

				// /proc/softirqs → rates chart
				if sirqSnaps := snaps["cat /proc/softirqs"]; len(sirqSnaps) > 1 {
					sirqSeries := processSoftirqSnapshots(sirqSnaps)

					var sirqKeys []string
					for k := range sirqSeries {
						sirqKeys = append(sirqKeys, k)
					}
					sort.Strings(sirqKeys)
					// Put totals first
					var orderedSirq []string
					for _, k := range sirqKeys {
						if strings.Contains(k, "total") {
							orderedSirq = append(orderedSirq, k)
						}
					}
					for _, k := range sirqKeys {
						if !strings.Contains(k, "total") {
							orderedSirq = append(orderedSirq, k)
						}
					}

					addSeriesOrdered(&rateData, sysName, sirqSeries, orderedSirq)
				}

				// /proc/net/softnet_stat → rates chart
				if snetSnaps := snaps["cat /proc/net/softnet_stat"]; len(snetSnaps) > 1 {
					snetSeries := processSoftnetStatSnapshots(snetSnaps)

					var snetKeys []string
					for k := range snetSeries {
						snetKeys = append(snetKeys, k)
					}
					sort.Strings(snetKeys)
					var orderedSnet []string
					for _, k := range snetKeys {
						if strings.Contains(k, "total") {
							orderedSnet = append(orderedSnet, k)
						}
					}
					for _, k := range snetKeys {
						if !strings.Contains(k, "total") {
							orderedSnet = append(orderedSnet, k)
						}
					}

					addSeriesOrdered(&rateData, sysName, snetSeries, orderedSnet)
				}

				// ps auxwww → QEMU/vhost thread CPU % chart
				if psSnaps := snaps["ps auxwww"]; len(psSnaps) > 1 {
					qemuSeries := processQemuThreadSnapshots(psSnaps)
					if len(qemuSeries) > 0 {
						var qemuKeys []string
						for k := range qemuSeries {
							qemuKeys = append(qemuKeys, k)
						}
						sort.Slice(qemuKeys, func(i, j int) bool {
							// qemu processes first, then others
							iQ := strings.HasPrefix(qemuKeys[i], "qemu")
							jQ := strings.HasPrefix(qemuKeys[j], "qemu")
							if iQ != jQ {
								return iQ
							}
							return qemuKeys[i] < qemuKeys[j]
						})
						qemuData = cpuSystemData{Name: sysName + " - QEMU & vhost Thread CPU %"}
						addSeriesOrdered(&qemuData, sysName, qemuSeries, qemuKeys)
					}
				}
			}
		}

		// Cloud API sources → CPU % chart
		type job struct {
			label string
			path  string
			parse func(string) ([]cpuDataPoint, error)
		}
		jobs := []job{
			{"status/cpu_util @10s", fs.statusCPU10s, parseStatusCPUUtil},
			{"status/cpu_util @60s", fs.statusCPU60s, parseStatusCPUUtil},
			{"timeSeries/CPU_USAGE", fs.timeSeriesUsage, parseTimeSeriesJSON},
			{"timeSeries/CPU_TOTAL", fs.timeSeriesTotal, parseTimeSeriesJSON},
		}
		for _, j := range jobs {
			if j.path == "" {
				continue
			}
			points, err := j.parse(j.path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", j.path, err)
				continue
			}
			if len(points) > 0 {
				cpuPctData.Series = append(cpuPctData.Series, cpuDataSeries{Name: j.label, Points: points})
				fmt.Fprintf(os.Stderr, "  %s / %s: %d data points\n", sysName, j.label, len(points))
			}
		}

		if len(cpuPctData.Series) > 0 {
			systems = append(systems, cpuPctData)
		}
		if len(rateData.Series) > 0 {
			systems = append(systems, rateData)
		}
		if len(qemuData.Series) > 0 {
			systems = append(systems, qemuData)
		}
	}

	if len(systems) == 0 {
		return fmt.Errorf("no valid CPU data found")
	}

	outputPath := filepath.Join(dir, "cpu_comparison.html")
	if err := generateComparisonHTML(systems, outputPath); err != nil {
		return fmt.Errorf("generating HTML: %w", err)
	}

	fmt.Printf("Generated CPU comparison chart: %s\n", outputPath)
	return nil
}

// --- HTML generation ---

type chartPoint struct {
	X int64   `json:"x"`
	Y float64 `json:"y"`
}

type chartDataset struct {
	Label string       `json:"label"`
	Data  []chartPoint `json:"data"`
}

type chartSystem struct {
	Name     string         `json:"name"`
	Datasets []chartDataset `json:"datasets"`
}

// --- Shared HTML helper functions ---

func writeHTMLHead(b *strings.Builder, title string) {
	b.WriteString(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>`)
	b.WriteString(title)
	b.WriteString(`</title>
  <script src="https://cdn.jsdelivr.net/npm/chart.js@4"></script>
  <script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3"></script>
  <script src="https://cdn.jsdelivr.net/npm/hammerjs@2"></script>
  <script src="https://cdn.jsdelivr.net/npm/chartjs-plugin-zoom@2"></script>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; margin: 20px; background: #f5f5f5; }
    h1 { text-align: center; color: #333; }
    .subtitle { text-align: center; color: #888; font-size: 0.9em; margin-bottom: 30px; }
    .chart-container {
      width: 95%; max-width: 1600px; margin: 30px auto;
      background: white; padding: 24px; border-radius: 8px;
      box-shadow: 0 2px 8px rgba(0,0,0,0.08);
    }
    .chart-container h2 { margin-top: 0; color: #444; font-size: 1.1em; }
    .series-info { font-size: 0.82em; color: #999; margin-bottom: 12px; }
    .btn { margin-top: 8px; padding: 5px 14px; cursor: pointer; border: 1px solid #ccc;
           border-radius: 4px; background: #fff; font-size: 0.85em; }
    .btn:hover { background: #eee; }
  </style>
</head>
<body>
`)
}

func writeChartJSConstants(b *strings.Builder) {
	b.WriteString(`
  // Deterministic color for a series label — same label always gets the same color.
  const PALETTE = [
    'rgb(54,162,235)', 'rgb(34,180,34)', 'rgb(220,40,40)', 'rgb(180,180,50)',
    'rgb(255,120,0)', 'rgb(160,50,200)', 'rgb(255,80,160)', 'rgb(0,190,190)',
    'rgb(128,128,128)', 'rgb(100,60,30)', 'rgb(180,100,200)', 'rgb(50,50,50)',
    'rgb(0,128,0)', 'rgb(200,0,200)', 'rgb(100,100,255)', 'rgb(200,120,60)',
    'rgb(60,60,180)', 'rgb(180,60,60)', 'rgb(60,180,120)', 'rgb(220,180,0)',
    'rgb(80,200,200)', 'rgb(200,80,200)', 'rgb(120,200,80)', 'rgb(80,80,200)',
  ];

  // Well-known labels get fixed colors for consistency across charts.
  const FIXED = {
    '/proc/stat (iowait=busy)': 'rgb(54,162,235)',
    '/proc/stat (iowait=idle)': 'rgb(34,180,34)',
    '/proc/stat user':    'rgb(220,40,40)',
    '/proc/stat nice':    'rgb(180,180,50)',
    '/proc/stat system':  'rgb(255,120,0)',
    '/proc/stat iowait':  'rgb(160,50,200)',
    '/proc/stat irq':     'rgb(255,80,160)',
    '/proc/stat softirq': 'rgb(0,190,190)',
    '/proc/stat steal':   'rgb(128,128,128)',
    '/proc/stat guest':   'rgb(100,60,30)',
    '/proc/stat guest_nice': 'rgb(180,100,200)',
    'status/cpu_util @10s':  'rgb(50,50,50)',
    'status/cpu_util @60s':  'rgb(0,128,0)',
    'timeSeries/CPU_USAGE':  'rgb(200,0,200)',
    'timeSeries/CPU_TOTAL':  'rgb(100,100,255)',
    'softirqs NET_RX total /s': 'rgb(220,40,40)',
    'softirqs NET_TX total /s': 'rgb(54,162,235)',
    'softnet processed total /s': 'rgb(34,180,34)',
    'softnet drops total /s': 'rgb(255,0,0)',
    'softnet squeeze total /s': 'rgb(255,120,0)',
  };

  // Per-CPU colors: use a distinct hue per CPU index.
  const CPU_HUES = [210, 0, 120, 45, 270, 170, 330, 90, 30, 300, 150, 60];

  function colorFor(label, idx) {
    if (FIXED[label]) return FIXED[label];
    // Per-CPU series: extract cpu number for a unique hue
    const cpuMatch = label.match(/cpu(\d+)/);
    if (cpuMatch) {
      const cpuIdx = parseInt(cpuMatch[1]);
      const hue = CPU_HUES[cpuIdx % CPU_HUES.length];
      // Vary lightness by metric type to distinguish softirq%/NET_RX/softnet
      let lit = 45;
      if (label.includes('softirqs')) lit = 40;
      if (label.includes('softnet')) lit = 55;
      if (label.includes('softirq %')) lit = 50;
      return 'hsl(' + hue + ',70%,' + lit + '%)';
    }
    return PALETTE[idx % PALETTE.length];
  }

  function alphaOf(color) {
    if (color.startsWith('hsl')) return color.replace(')', ',0.06)').replace('hsl', 'hsla');
    return color.replace('rgb', 'rgba').replace(')', ',0.06)');
  }

  // Which series start visible?
  function defaultVisible(label) {
    // Aggregates and cloud API: visible
    if (label.includes('iowait=busy') || label.includes('iowait=idle')) return true;
    if (label.startsWith('status/') || label.startsWith('timeSeries/')) return true;
    // Rate chart totals: visible
    if (label.includes('total /s')) return true;
    // QEMU/vhost threads: visible
    if (label.startsWith('qemu ') || label.startsWith('vhost-')) return true;
    // sysfs drop/error series: visible (these only appear when non-zero)
    if (label.includes('sysfs') && (label.includes('dropped') || label.includes('errors'))) return true;
    return false;
  }

  const charts = [];
  let syncing = false;

  function syncCharts(sourceChart) {
    if (syncing) return;
    syncing = true;
    const srcX = sourceChart.scales.x;
    charts.forEach(c => {
      if (c === sourceChart) return;
      c.options.scales.x.min = srcX.min;
      c.options.scales.x.max = srcX.max;
      c.update('none');
    });
    syncing = false;
  }

  function resetAllZoom() {
    syncing = true;
    charts.forEach(c => { c.resetZoom(); });
    syncing = false;
  }
`)
}

func writeTimeSeriesChartLoop(b *strings.Builder, dataVar string) {
	b.WriteString(fmt.Sprintf(`
  %s.forEach((sys, idx) => {
    const canvas = document.getElementById('chart' + idx);
    const yLabel = canvas.getAttribute('data-ylabel') || 'Value';
    const datasets = sys.datasets.map((ds, di) => {
      const c = colorFor(ds.label, di);
      const vis = defaultVisible(ds.label);
      return {
        label: ds.label,
        data: ds.data,
        borderColor: c,
        backgroundColor: alphaOf(c),
        borderWidth: vis ? 1.5 : 1,
        pointRadius: ds.data.length < 100 ? 2 : 0,
        pointHitRadius: 8,
        tension: 0.1,
        fill: false,
        hidden: !vis,
      };
    });

    const chart = new Chart(canvas, {
      type: 'line',
      data: { datasets },
      options: {
        responsive: true,
        animation: false,
        interaction: { mode: 'nearest', axis: 'x', intersect: false },
        scales: {
          x: {
            type: 'time',
            time: {
              tooltipFormat: 'yyyy-MM-dd HH:mm:ss',
              displayFormats: { second: 'HH:mm:ss', minute: 'HH:mm', hour: 'HH:mm' },
            },
            title: { display: true, text: 'Time (UTC)' },
          },
          y: {
            title: { display: true, text: yLabel },
            beginAtZero: true,
          }
        },
        plugins: {
          zoom: {
            zoom: {
              wheel: {enabled: true}, pinch: {enabled: true}, mode: 'x',
              onZoom: function({chart}) { syncCharts(chart); },
            },
            pan: {
              enabled: true, mode: 'x',
              onPan: function({chart}) { syncCharts(chart); },
            },
          },
          tooltip: {
            callbacks: {
              title: function(items) {
                if (items.length > 0) {
                  const d = new Date(items[0].parsed.x);
                  return d.toISOString().replace('T',' ').replace('.000Z',' UTC');
                }
                return '';
              },
              label: function(item) {
                const v = item.parsed.y;
                const fmt = v > 100 ? v.toFixed(0) : v.toFixed(2);
                return item.dataset.label + ': ' + fmt;
              }
            }
          }
        }
      }
    });
    charts.push(chart);
  });
`, dataVar))
}

func writeHTMLEnd(b *strings.Builder) {
	b.WriteString("  </script>\n</body>\n</html>\n")
}

// systemsToChartData converts cpuSystemData to chartSystem format for JSON serialization.
func systemsToChartData(systems []cpuSystemData) []chartSystem {
	var chartData []chartSystem
	for _, sys := range systems {
		cs := chartSystem{Name: sys.Name}
		for _, series := range sys.Series {
			ds := chartDataset{Label: series.Name}
			for _, p := range series.Points {
				ds.Data = append(ds.Data, chartPoint{
					X: p.Time.UnixMilli(),
					Y: p.Value,
				})
			}
			cs.Datasets = append(cs.Datasets, ds)
		}
		chartData = append(chartData, cs)
	}
	return chartData
}

func generateComparisonHTML(systems []cpuSystemData, outputPath string) error {
	chartData := systemsToChartData(systems)

	dataJSON, err := json.Marshal(chartData)
	if err != nil {
		return err
	}

	var b strings.Builder

	writeHTMLHead(&b, "CPU Utilization Comparison")

	b.WriteString("  <h1>CPU Utilization Comparison</h1>\n")
	b.WriteString("  <p class=\"subtitle\">Scroll to zoom | Drag to pan | Click legend to toggle series</p>\n")

	for i, sys := range chartData {
		b.WriteString(fmt.Sprintf("  <div class=\"chart-container\">\n    <h2>%s</h2>\n    <div class=\"series-info\">", sys.Name))
		for j, ds := range sys.Datasets {
			if j > 0 {
				b.WriteString(" &middot; ")
			}
			b.WriteString(fmt.Sprintf("%s (%d pts)", ds.Label, len(ds.Data)))
		}
		yLabel := "CPU %"
		if strings.Contains(sys.Name, "Rates") {
			yLabel = "events/s"
		}
		b.WriteString(fmt.Sprintf("</div>\n    <canvas id=\"chart%d\" height=\"110\" data-ylabel=\"%s\"></canvas>\n", i, yLabel))
		b.WriteString("    <button class=\"btn\" onclick=\"resetAllZoom()\">Reset Zoom</button>\n  </div>\n\n")
	}

	b.WriteString("  <script>\n  const DATA = ")
	b.Write(dataJSON)
	b.WriteString(";\n")

	writeChartJSConstants(&b)
	writeTimeSeriesChartLoop(&b, "DATA")
	writeHTMLEnd(&b)

	return os.WriteFile(outputPath, []byte(b.String()), 0o644)
}
