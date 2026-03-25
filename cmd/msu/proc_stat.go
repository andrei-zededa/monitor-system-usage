package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorYellow = "\033[33m"
	colorOrange = "\033[38;5;208m" // 256-color orange
	colorRed    = "\033[31m"
)

// getColorForPercent returns the appropriate color code based on the percentage
// and whether it's an idle field (which has reversed thresholds).
func getColorForPercent(percent float64, isIdle bool) string {
	if isIdle {
		// Reverse logic for idle: low idle is bad
		if percent <= 20 {
			return colorRed
		} else if percent <= 50 {
			return colorOrange
		} else if percent <= 80 {
			return colorYellow
		}
		return ""
	}

	// Normal logic for usage percentages: high usage is bad
	if percent > 80 {
		return colorRed
	} else if percent > 50 {
		return colorOrange
	} else if percent > 20 {
		return colorYellow
	}
	return ""
}

// PrintCPUAndSoftIRQStats calculates and displays per-core CPU usage (by type)
// and softirq rates between two snapshots of /proc/stat.
//
// Arguments:
//
//	prevLines: contents of /proc/stat at prevTime, split by line
//	currLines: contents of /proc/stat at currTime, split by line
//	prevTime:  time when prevLines was captured
//	currTime:  time when currLines was captured
//	hz:        number of jiffies per second (e.g. 100, 250, 1000)
func PrintCPUAndSoftIRQStats(prevLines, currLines []string, prevTime, currTime time.Time, hz float64) {
	if hz <= 0 {
		fmt.Println("invalid HZ value; must be > 0")
		return
	}

	interval := currTime.Sub(prevTime).Seconds()
	if interval <= 0 {
		fmt.Println("invalid time interval; currTime must be after prevTime")
		return
	}

	// Parse CPU and softirq data from both snapshots.
	prevCPU := parseCPUStats(prevLines)
	currCPU := parseCPUStats(currLines)

	prevSoft := parseSoftIRQ(prevLines)
	currSoft := parseSoftIRQ(currLines)

	fmt.Printf("PrintCPUAndSoftIRQStats    Time interval: %.3f secs\n", interval)
	fmt.Printf("==== CPU usage per core (over interval) ====\n")

	// Names for CPU time fields in /proc/stat
	cpuFieldNames := []string{
		"user", "nice", "system", "idle",
		"iowait", "irq", "softirq", "steal",
		"guest", "guest_nice",
	}

	// Collect and sort CPU IDs for stable output order
	cpuIDs := make([]string, 0, len(currCPU))
	for cpuID := range currCPU {
		cpuIDs = append(cpuIDs, cpuID)
	}
	sort.Slice(cpuIDs, func(i, j int) bool {
		// "cpu" (aggregate) always comes first
		if cpuIDs[i] == "cpu" {
			return true
		}
		if cpuIDs[j] == "cpu" {
			return false
		}
		// Extract numeric suffix and compare numerically
		numI, errI := strconv.Atoi(strings.TrimPrefix(cpuIDs[i], "cpu"))
		numJ, errJ := strconv.Atoi(strings.TrimPrefix(cpuIDs[j], "cpu"))
		if errI != nil || errJ != nil {
			// Fallback to string comparison if parsing fails
			return cpuIDs[i] < cpuIDs[j]
		}
		return numI < numJ
	})

	// For each CPU present in both snapshots
	for _, cpuID := range cpuIDs {
		currVals := currCPU[cpuID]
		prevVals, ok := prevCPU[cpuID]
		if !ok {
			continue
		}

		n := len(currVals)
		if len(prevVals) < n {
			n = len(prevVals)
		}
		if n == 0 {
			continue
		}

		// Compute deltas
		deltas := make([]uint64, n)
		var totalDelta uint64
		for i := 0; i < n; i++ {
			if currVals[i] >= prevVals[i] {
				deltas[i] = currVals[i] - prevVals[i]
			} else {
				// counter wrapped or reset; ignore this field
				deltas[i] = 0
			}
			totalDelta += deltas[i]
		}
		if totalDelta == 0 {
			continue
		}

		// Print CPU ID and all stats on a single line
		fmt.Printf("%-6s: ", cpuID)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("field_%d", i)
			if i < len(cpuFieldNames) {
				name = cpuFieldNames[i]
			}

			// Percentage of total CPU time for this CPU over the interval
			percent := (float64(deltas[i]) / float64(totalDelta)) * 100.0

			if i > 0 {
				fmt.Print(" ")
			}

			// Determine if this is the idle field for reverse color logic
			isIdle := (name == "idle")
			color := getColorForPercent(percent, isIdle)

			// Print with color if applicable
			if color != "" {
				fmt.Printf("%s=%s%6.2f%%%s", name, color, percent, colorReset)
			} else {
				fmt.Printf("%s=%6.2f%%", name, percent)
			}
		}
		fmt.Println()
	}

	// --- SoftIRQ stats ---
	fmt.Println("\n==== SoftIRQ rate of change (events per second) ====")

	if len(prevSoft) == 0 || len(currSoft) == 0 {
		fmt.Println("no softirq line found in one or both snapshots")
		return
	}

	softirqNames := []string{
		"HI", "TIMER", "NET_TX", "NET_RX",
		"BLOCK", "IRQ_POLL", "TASKLET", "SCHED",
		"HRTIMER", "RCU",
	}

	m := len(currSoft)
	if len(prevSoft) < m {
		m = len(prevSoft)
	}
	if m == 0 {
		fmt.Println("softirq data incomplete")
		return
	}

	// Print all softirq stats on a single line
	fmt.Print("SoftIRQ: ")

	// Index 0 is total softirqs
	if currSoft[0] >= prevSoft[0] {
		deltaTotal := currSoft[0] - prevSoft[0]
		rateTotal := float64(deltaTotal) / interval
		fmt.Printf("total=%d(%.2f/s)", deltaTotal, rateTotal)
	}

	// Per-softirq-type rates
	for i := 1; i < m; i++ {
		if currSoft[i] < prevSoft[i] {
			continue
		}
		delta := currSoft[i] - prevSoft[i]
		rate := float64(delta) / interval

		name := fmt.Sprintf("softirq_%d", i-1)
		if i-1 < len(softirqNames) {
			name = softirqNames[i-1]
		}

		fmt.Printf(" %s=%d(%.2f/s)", name, delta, rate)
	}
	fmt.Printf("\n\n")
}

// parseCPUStats extracts CPU stat lines ("cpu", "cpu0", "cpu1", ...) into a map.
func parseCPUStats(lines []string) map[string][]uint64 {
	result := make(map[string][]uint64)

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		label := fields[0]
		if label == "cpu" || strings.HasPrefix(label, "cpu") && label != "softirq" {
			// Parse numeric fields
			vals := make([]uint64, 0, len(fields)-1)
			for _, f := range fields[1:] {
				v, err := strconv.ParseUint(f, 10, 64)
				if err != nil {
					// On error, stop parsing this line
					vals = nil
					break
				}
				vals = append(vals, v)
			}
			if vals != nil {
				result[label] = vals
			}
		}
	}

	return result
}

// parseSoftIRQ extracts the "softirq" line as a slice of uint64:
// [total, HI, TIMER, NET_TX, NET_RX, BLOCK, IRQ_POLL, TASKLET, SCHED, HRTIMER, RCU]
func parseSoftIRQ(lines []string) []uint64 {
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "softirq" {
			vals := make([]uint64, 0, len(fields)-1)
			for _, f := range fields[1:] {
				v, err := strconv.ParseUint(f, 10, 64)
				if err != nil {
					return nil
				}
				vals = append(vals, v)
			}
			return vals
		}
	}
	return nil
}
