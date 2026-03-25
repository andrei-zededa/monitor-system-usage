package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// --- iperf/params JSON types ---

type iperfAggregate struct {
	AchievedClientKpps float64 `json:"achieved_client_kpps"`
	AchievedServerKpps float64 `json:"achieved_server_kpps"`
	LossPercent        float64 `json:"loss_percent"`
}

type iperfFlowResult struct {
	FlowID             int     `json:"flow_id"`
	TargetKpps         int     `json:"target_kpps"`
	AchievedClientKpps float64 `json:"achieved_client_kpps,omitempty"`
	AchievedServerKpps float64 `json:"achieved_server_kpps,omitempty"`
	LossPercent        float64 `json:"loss_percent,omitempty"`
}

type iperfResult struct {
	TargetKpps         int              `json:"target_kpps"`
	AchievedClientKpps float64          `json:"achieved_client_kpps,omitempty"`
	AchievedServerKpps float64          `json:"achieved_server_kpps,omitempty"`
	LossPercent        float64          `json:"loss_percent,omitempty"`
	StartTime          string           `json:"start_time,omitempty"`
	EndTime            string           `json:"end_time,omitempty"`
	FlowCount          int              `json:"flow_count,omitempty"`
	Aggregate          *iperfAggregate  `json:"aggregate,omitempty"`
	PerFlow            []iperfFlowResult `json:"per_flow,omitempty"`
}

type iperfData struct {
	MultiFlow   bool          `json:"multi_flow"`
	RatesTested []int         `json:"rates_tested"`
	Results     []iperfResult `json:"results"`
}

type testParams struct {
	ClientInterface string `json:"client_interface"`
	ServerInterface string `json:"server_interface"`
	ClientNamespace string `json:"client_namespace"`
	ServerNamespace string `json:"server_namespace"`
	ClientCores     []int  `json:"client_cores"`
	ServerCores     []int  `json:"server_cores"`
	TestDurationSec int    `json:"test_duration_sec"`
	TestName        string `json:"test_name"`
}

// --- Rate-step data types ---

type rateStepPoint struct {
	TargetKpps         int
	AchievedClientKpps float64
	AchievedServerKpps float64
	LossPercent        float64
}

type rateStepCPU struct {
	TargetKpps int
	CoreData   map[string]map[string]float64 // core -> field -> pct
}

type rateStepSoftirq struct {
	TargetKpps int
	NetRX      float64
	NetTX      float64
}

// --- Orchestration ---

func runReport(dir, iperfPath, paramsPath string) error {
	fileSets, err := discoverCPUCompareFiles(dir)
	if err != nil {
		return fmt.Errorf("discovering files: %w", err)
	}
	if len(fileSets) == 0 {
		return fmt.Errorf("no MSU data files found in %s", dir)
	}

	// Determine analysis level
	hasCloudAPI := false
	multipleSystemsWithMSU := 0
	for _, fs := range fileSets {
		if fs.statusCPU10s != "" || fs.statusCPU60s != "" || fs.timeSeriesUsage != "" || fs.timeSeriesTotal != "" {
			hasCloudAPI = true
		}
		if fs.msuOut != "" {
			multipleSystemsWithMSU++
		}
	}
	hasIperf := iperfPath != ""
	hasCrossVM := multipleSystemsWithMSU > 1

	level := 1
	if hasCloudAPI {
		level = 2
	}
	if hasCrossVM {
		level = 3
	}
	if hasIperf {
		level = 4
	}

	// Process time-series data (same as cpu-compare + net/dev)
	var timeSeriesSystems []cpuSystemData

	var sysNames []string
	for name := range fileSets {
		sysNames = append(sysNames, name)
	}
	sort.Strings(sysNames)

	for _, sysName := range sysNames {
		fs := fileSets[sysName]
		cpuPctData := cpuSystemData{Name: sysName}
		rateData := cpuSystemData{Name: sysName + " - Softirq & Network Rates"}
		netData := cpuSystemData{Name: sysName + " - Interface Traffic & Drops"}
		var qemuData cpuSystemData

		if fs.msuOut != "" {
			// Build command set: standard commands + discovered sysfs stat commands
			commands := map[string]bool{
				"cat /proc/stat":             true,
				"cat /proc/softirqs":         true,
				"cat /proc/net/softnet_stat": true,
				"ps auxwww":                  true,
				"cat /proc/net/dev":          true,
			}
			for cmd := range discoverSysfsStatCommands(fs.msuOut) {
				commands[cmd] = true
			}
			snaps, err := collectMSUSnapshotsAuto(fs.msuOut, commands)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", fs.msuOut, err)
			} else {
				// /proc/stat → CPU %
				if statSnaps := snaps["cat /proc/stat"]; len(statSnaps) > 1 {
					statSeries := processProcStatSnapshots(statSnaps)
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

				// /proc/softirqs → rates
				if sirqSnaps := snaps["cat /proc/softirqs"]; len(sirqSnaps) > 1 {
					sirqSeries := processSoftirqSnapshots(sirqSnaps)
					var sirqKeys []string
					for k := range sirqSeries {
						sirqKeys = append(sirqKeys, k)
					}
					sort.Strings(sirqKeys)
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

				// /proc/net/softnet_stat → rates
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

				// ps auxwww → QEMU/vhost thread CPU %
				if psSnaps := snaps["ps auxwww"]; len(psSnaps) > 1 {
					qemuSeries := processQemuThreadSnapshots(psSnaps)
					if len(qemuSeries) > 0 {
						var qemuKeys []string
						for k := range qemuSeries {
							qemuKeys = append(qemuKeys, k)
						}
						sort.Slice(qemuKeys, func(i, j int) bool {
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

				// /proc/net/dev → interface traffic
				if devSnaps := snaps["cat /proc/net/dev"]; len(devSnaps) > 1 {
					devSeries := processNetDevSnapshots(devSnaps)
					if len(devSeries) > 0 {
						var devKeys []string
						for k := range devSeries {
							devKeys = append(devKeys, k)
						}
						sort.Strings(devKeys)
						// Put pps and Mbps first, drops/errors last
						var orderedDev []string
						for _, k := range devKeys {
							if strings.HasSuffix(k, "_pps") || strings.HasSuffix(k, "_Mbps") {
								orderedDev = append(orderedDev, k)
							}
						}
						for _, k := range devKeys {
							if strings.HasSuffix(k, "/s") {
								orderedDev = append(orderedDev, k)
							}
						}
						addSeriesOrdered(&netData, sysName, devSeries, orderedDev)
					}
				}

				// sysfs /sys/class/net/*/statistics/* → interface drop/error rates
				sysfsSeries := processSysfsStatSnapshots(snaps)
				if len(sysfsSeries) > 0 {
					var sysfsKeys []string
					for k := range sysfsSeries {
						sysfsKeys = append(sysfsKeys, k)
					}
					sort.Strings(sysfsKeys)
					addSeriesOrdered(&netData, sysName, sysfsSeries, sysfsKeys)
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
			timeSeriesSystems = append(timeSeriesSystems, cpuPctData)
		}
		if len(rateData.Series) > 0 {
			timeSeriesSystems = append(timeSeriesSystems, rateData)
		}
		if len(netData.Series) > 0 {
			timeSeriesSystems = append(timeSeriesSystems, netData)
		}
		if len(qemuData.Series) > 0 {
			timeSeriesSystems = append(timeSeriesSystems, qemuData)
		}
	}

	// Parse iperf data if provided
	var iperf *iperfData
	var params *testParams
	var rateSteps []rateStepPoint
	var rateStepCPUs []rateStepCPU
	var rateStepSirqs []rateStepSoftirq

	if iperfPath != "" {
		raw, err := os.ReadFile(iperfPath)
		if err != nil {
			return fmt.Errorf("reading iperf JSON: %w", err)
		}
		iperf = &iperfData{}
		if err := json.Unmarshal(raw, iperf); err != nil {
			return fmt.Errorf("parsing iperf JSON: %w", err)
		}

		if paramsPath != "" {
			raw, err := os.ReadFile(paramsPath)
			if err != nil {
				return fmt.Errorf("reading params JSON: %w", err)
			}
			params = &testParams{}
			if err := json.Unmarshal(raw, params); err != nil {
				return fmt.Errorf("parsing params JSON: %w", err)
			}
		}

		// Build rate-step throughput data
		for _, r := range iperf.Results {
			pt := rateStepPoint{TargetKpps: r.TargetKpps}
			if iperf.MultiFlow && r.Aggregate != nil {
				pt.AchievedClientKpps = r.Aggregate.AchievedClientKpps
				pt.AchievedServerKpps = r.Aggregate.AchievedServerKpps
				pt.LossPercent = r.Aggregate.LossPercent
			} else {
				pt.AchievedClientKpps = r.AchievedClientKpps
				pt.AchievedServerKpps = r.AchievedServerKpps
				pt.LossPercent = r.LossPercent
			}
			rateSteps = append(rateSteps, pt)
		}

		// Compute rate-step CPU and softirq metrics from MSU data
		// Use the first system's msu.out that we can find
		var msuFile string
		for _, name := range sysNames {
			if fileSets[name].msuOut != "" {
				msuFile = fileSets[name].msuOut
				break
			}
		}
		if msuFile != "" {
			snaps, err := collectMSUSnapshotsAuto(msuFile, map[string]bool{
				"cat /proc/stat":     true,
				"cat /proc/softirqs": true,
			})
			if err == nil {
				statSnaps := snaps["cat /proc/stat"]
				sirqSnaps := snaps["cat /proc/softirqs"]

				for _, r := range iperf.Results {
					startT, endT := parseIperfTimeWindow(r, params)
					if startT.IsZero() || endT.IsZero() {
						continue
					}

					// CPU per rate step
					if len(statSnaps) > 1 {
						cpuData := computeRateStepCPU(statSnaps, startT, endT)
						cpuData.TargetKpps = r.TargetKpps
						rateStepCPUs = append(rateStepCPUs, cpuData)
					}

					// Softirq per rate step
					if len(sirqSnaps) > 1 {
						sirqData := computeRateStepSoftirq(sirqSnaps, startT, endT)
						sirqData.TargetKpps = r.TargetKpps
						rateStepSirqs = append(rateStepSirqs, sirqData)
					}
				}
			}
		}
	}

	// Generate HTML
	outputPath := filepath.Join(dir, "report.html")
	if err := generateReportHTML(outputPath, level, timeSeriesSystems, rateSteps, rateStepCPUs, rateStepSirqs, params); err != nil {
		return fmt.Errorf("generating report HTML: %w", err)
	}

	fmt.Printf("Generated MSU analysis report: %s\n", outputPath)
	return nil
}

// parseIperfTimeWindow extracts the effective analysis time window for a rate step.
func parseIperfTimeWindow(r iperfResult, params *testParams) (time.Time, time.Time) {
	if r.StartTime == "" || r.EndTime == "" {
		return time.Time{}, time.Time{}
	}

	startT, err := time.Parse("2006-01-02 15:04:05", r.StartTime)
	if err != nil {
		return time.Time{}, time.Time{}
	}

	// Skip first 2 seconds (startup transient)
	startT = startT.Add(2 * time.Second)

	endT, err := time.Parse("2006-01-02 15:04:05", r.EndTime)
	if err != nil {
		return time.Time{}, time.Time{}
	}

	// Use test duration if available (avoid post-test idle periods)
	if params != nil && params.TestDurationSec > 0 {
		effectiveEnd := startT.Add(time.Duration(params.TestDurationSec-2) * time.Second)
		if effectiveEnd.Before(endT) {
			endT = effectiveEnd
		}
	}

	return startT, endT
}

// computeRateStepCPU computes average per-core CPU percentages within a time window.
func computeRateStepCPU(statSnaps []msuSnapshot, startT, endT time.Time) rateStepCPU {
	result := rateStepCPU{CoreData: make(map[string]map[string]float64)}
	count := 0

	for i := 1; i < len(statSnaps); i++ {
		t := statSnaps[i].time
		if t.Before(startT) || t.After(endT) {
			continue
		}

		prevCPU := parseCPUStats(statSnaps[i-1].lines)
		currCPU := parseCPUStats(statSnaps[i].lines)

		for cpuName, currVals := range currCPU {
			prevVals, ok := prevCPU[cpuName]
			if !ok {
				continue
			}
			n := len(currVals)
			if len(prevVals) < n {
				n = len(prevVals)
			}
			if n < 5 {
				continue
			}

			deltas := make([]uint64, n)
			var totalDelta uint64
			for j := 0; j < n; j++ {
				if currVals[j] >= prevVals[j] {
					deltas[j] = currVals[j] - prevVals[j]
				}
				totalDelta += deltas[j]
			}
			if totalDelta == 0 {
				continue
			}

			if result.CoreData[cpuName] == nil {
				result.CoreData[cpuName] = make(map[string]float64)
			}
			for j := 0; j < n && j < len(procStatFields); j++ {
				pct := float64(deltas[j]) / float64(totalDelta) * 100.0
				result.CoreData[cpuName][procStatFields[j]] += pct
			}
		}
		count++
	}

	// Average
	if count > 0 {
		for _, fields := range result.CoreData {
			for k := range fields {
				fields[k] /= float64(count)
			}
		}
	}

	return result
}

// computeRateStepSoftirq computes average NET_RX/NET_TX softirq rates within a time window.
func computeRateStepSoftirq(sirqSnaps []msuSnapshot, startT, endT time.Time) rateStepSoftirq {
	var result rateStepSoftirq
	var rxTotal, txTotal float64
	count := 0

	for i := 1; i < len(sirqSnaps); i++ {
		t := sirqSnaps[i].time
		if t.Before(startT) || t.After(endT) {
			continue
		}

		dt := sirqSnaps[i].time.Sub(sirqSnaps[i-1].time).Seconds()
		if dt <= 0 {
			continue
		}

		prev, numCPUs := parseSoftirqsTable(sirqSnaps[i-1].lines)
		curr, _ := parseSoftirqsTable(sirqSnaps[i].lines)
		if prev == nil || curr == nil {
			continue
		}

		for _, typeName := range []string{"NET_RX", "NET_TX"} {
			pv, ok1 := prev[typeName]
			cv, ok2 := curr[typeName]
			if !ok1 || !ok2 {
				continue
			}
			var totalRate float64
			for cpu := 0; cpu < numCPUs && cpu < len(pv) && cpu < len(cv); cpu++ {
				if cv[cpu] >= pv[cpu] {
					totalRate += float64(cv[cpu]-pv[cpu]) / dt
				}
			}
			if typeName == "NET_RX" {
				rxTotal += totalRate
			} else {
				txTotal += totalRate
			}
		}
		count++
	}

	if count > 0 {
		result.NetRX = rxTotal / float64(count)
		result.NetTX = txTotal / float64(count)
	}
	return result
}

// --- HTML Report Generation ---

func generateReportHTML(outputPath string, level int, timeSeriesSystems []cpuSystemData,
	rateSteps []rateStepPoint, rateStepCPUs []rateStepCPU, rateStepSirqs []rateStepSoftirq, params *testParams) error {

	// Convert time-series data
	chartData := systemsToChartData(timeSeriesSystems)
	tsJSON, err := json.Marshal(chartData)
	if err != nil {
		return err
	}

	var b strings.Builder

	writeHTMLHead(&b, "MSU Analysis Report")

	// Extra styles for the report
	b.WriteString(`  <style>
    .nav-bar { width: 95%; max-width: 1600px; margin: 0 auto 20px; background: white;
               padding: 12px 24px; border-radius: 8px; box-shadow: 0 2px 8px rgba(0,0,0,0.08); }
    .nav-bar a { color: #2196F3; text-decoration: none; margin-right: 18px; font-size: 0.9em; }
    .nav-bar a:hover { text-decoration: underline; }
    .level-badge { display: inline-block; padding: 3px 10px; border-radius: 12px;
                   font-size: 0.8em; font-weight: 600; margin-left: 12px; }
    .level-1 { background: #e3f2fd; color: #1565c0; }
    .level-2 { background: #e8f5e9; color: #2e7d32; }
    .level-3 { background: #fff3e0; color: #e65100; }
    .level-4 { background: #fce4ec; color: #c62828; }
    .section-header { width: 95%; max-width: 1600px; margin: 30px auto 10px; padding: 0 24px;
                      color: #666; font-size: 1.1em; border-bottom: 2px solid #ddd; }
    #analysis { width: 95%; max-width: 1600px; margin: 30px auto;
                background: white; padding: 24px 32px; border-radius: 8px;
                box-shadow: 0 2px 8px rgba(0,0,0,0.08); line-height: 1.6; }
    #analysis h2 { color: #333; margin-top: 24px; margin-bottom: 8px; font-size: 1.15em; }
    #analysis h3 { color: #444; margin-top: 18px; margin-bottom: 6px; font-size: 1.0em; }
    #analysis p { color: #444; margin: 6px 0; }
    #analysis ul, #analysis ol { color: #444; margin: 6px 0 6px 20px; }
    #analysis table { border-collapse: collapse; margin: 12px 0; width: 100%; }
    #analysis th { background: #f0f4f8; color: #333; text-align: left; padding: 8px 12px;
                   border: 1px solid #ddd; font-size: 0.9em; }
    #analysis td { padding: 6px 12px; border: 1px solid #ddd; font-size: 0.9em; color: #444; }
    #analysis tr:nth-child(even) { background: #fafafa; }
    #analysis code { background: #f0f0f0; padding: 1px 5px; border-radius: 3px; font-size: 0.9em; }
    #analysis pre { background: #f5f5f5; padding: 12px; border-radius: 6px; overflow-x: auto;
                    font-size: 0.85em; line-height: 1.4; }
    #analysis .finding { border-left: 4px solid #2196F3; padding: 8px 16px; margin: 12px 0;
                         background: #f8fbff; }
    #analysis .warning { border-left: 4px solid #FF9800; padding: 8px 16px; margin: 12px 0;
                         background: #fff8f0; }
    #analysis .critical { border-left: 4px solid #f44336; padding: 8px 16px; margin: 12px 0;
                          background: #fff5f5; }
    #analysis .ok { border-left: 4px solid #4CAF50; padding: 8px 16px; margin: 12px 0;
                    background: #f5fff5; }
    #analysis .diagram { background: #f8f8f8; border: 1px solid #e0e0e0; border-radius: 6px;
                         padding: 16px; margin: 12px 0; font-family: monospace;
                         white-space: pre; line-height: 1.3; overflow-x: auto; }
    .analysis-placeholder { color: #aaa; font-style: italic; }
  </style>
`)

	// Title
	levelLabels := map[int]string{
		1: "Single System MSU",
		2: "+ Zedcloud API",
		3: "+ Cross-VM",
		4: "+ iperf Tests",
	}
	b.WriteString(fmt.Sprintf("  <h1>MSU Analysis Report <span class=\"level-badge level-%d\">Level %d: %s</span></h1>\n", level, level, levelLabels[level]))
	b.WriteString("  <p class=\"subtitle\">Scroll to zoom | Drag to pan | Click legend to toggle series</p>\n")

	// Navigation
	b.WriteString("  <div class=\"nav-bar\">\n")
	if len(rateSteps) > 0 {
		b.WriteString("    <a href=\"#rate-step\">Rate-Step Analysis</a>\n")
	}
	b.WriteString("    <a href=\"#time-series\">Time-Series</a>\n")
	b.WriteString("    <a href=\"#analysis\">Analysis</a>\n")
	b.WriteString("  </div>\n\n")

	// Chart index for unique IDs
	chartIdx := 0

	// --- Rate-Step Section ---
	if len(rateSteps) > 0 {
		b.WriteString("  <div class=\"section-header\" id=\"rate-step\">Rate-Step Analysis</div>\n\n")

		// Throughput vs Load chart
		rsJSON, _ := json.Marshal(rateSteps)
		b.WriteString("  <div class=\"chart-container\">\n")
		b.WriteString("    <h2>Throughput vs Load</h2>\n")
		b.WriteString(fmt.Sprintf("    <canvas id=\"rs_throughput\" height=\"100\"></canvas>\n"))
		b.WriteString("  </div>\n\n")

		// CPU Usage vs Load chart (if we have rate-step CPU data and params)
		if len(rateStepCPUs) > 0 && params != nil {
			cpuJSON, _ := json.Marshal(rateStepCPUs)
			keyCores := make(map[int]string) // core num -> role
			for _, c := range params.ClientCores {
				keyCores[c] = "client"
			}
			for _, c := range params.ServerCores {
				if role, exists := keyCores[c]; exists {
					keyCores[c] = role + "+server"
				} else {
					keyCores[c] = "server"
				}
			}
			keyCoresJSON, _ := json.Marshal(keyCores)

			b.WriteString("  <div class=\"chart-container\">\n")
			b.WriteString("    <h2>CPU Usage vs Load (Key Cores)</h2>\n")
			b.WriteString("    <canvas id=\"rs_cpu\" height=\"120\"></canvas>\n")
			b.WriteString("  </div>\n\n")

			// SoftIRQ Rate vs Load
			if len(rateStepSirqs) > 0 {
				sirqJSON, _ := json.Marshal(rateStepSirqs)
				b.WriteString("  <div class=\"chart-container\">\n")
				b.WriteString("    <h2>SoftIRQ Rate vs Load</h2>\n")
				b.WriteString("    <canvas id=\"rs_softirq\" height=\"100\"></canvas>\n")
				b.WriteString("  </div>\n\n")

				// Embed rate-step JS data (softirq)
				_ = sirqJSON // used below in script
			}

			_ = cpuJSON     // used below
			_ = keyCoresJSON // used below
		}

		// Start script block for rate-step charts
		b.WriteString("  <script>\n")
		b.WriteString(fmt.Sprintf("  const RS_DATA = %s;\n", string(rsJSON)))

		// Throughput chart
		b.WriteString(`
  (function() {
    const ctx = document.getElementById('rs_throughput');
    const rates = RS_DATA.map(d => d.TargetKpps);
    const maxVal = Math.max(...rates, ...RS_DATA.map(d => d.AchievedClientKpps), ...RS_DATA.map(d => d.AchievedServerKpps)) * 1.1;
    new Chart(ctx, {
      type: 'line',
      data: {
        labels: rates,
        datasets: [
          { label: 'Ideal (y=x)', data: rates.map(r => r), borderColor: 'rgba(0,0,0,0.2)', borderDash: [5,5], pointRadius: 0, fill: false },
          { label: 'Client sent kpps', data: RS_DATA.map(d => d.AchievedClientKpps), borderColor: 'rgb(54,162,235)', backgroundColor: 'rgba(54,162,235,0.1)', pointRadius: 4, fill: false },
          { label: 'Server received kpps', data: RS_DATA.map(d => d.AchievedServerKpps), borderColor: 'rgb(220,40,40)', backgroundColor: 'rgba(220,40,40,0.1)', pointRadius: 4, fill: false },
          { label: 'Loss %', data: RS_DATA.map(d => d.LossPercent), borderColor: 'rgb(255,120,0)', backgroundColor: 'rgba(255,120,0,0.1)', pointRadius: 3, fill: false, yAxisID: 'y1', hidden: true },
        ]
      },
      options: {
        responsive: true, animation: false,
        scales: {
          x: { title: { display: true, text: 'Target kpps' } },
          y: { title: { display: true, text: 'kpps' }, beginAtZero: true },
          y1: { position: 'right', title: { display: true, text: 'Loss %' }, beginAtZero: true, grid: { drawOnChartArea: false } },
        }
      }
    });
  })();
`)

		// CPU vs Load chart
		if len(rateStepCPUs) > 0 && params != nil {
			cpuJSON, _ := json.Marshal(rateStepCPUs)
			keyCores := make(map[int]string)
			for _, c := range params.ClientCores {
				keyCores[c] = "client"
			}
			for _, c := range params.ServerCores {
				if role, exists := keyCores[c]; exists {
					keyCores[c] = role + "+server"
				} else {
					keyCores[c] = "server"
				}
			}
			keyCoresJSON, _ := json.Marshal(keyCores)

			b.WriteString(fmt.Sprintf("  const RS_CPU = %s;\n", string(cpuJSON)))
			b.WriteString(fmt.Sprintf("  const KEY_CORES = %s;\n", string(keyCoresJSON)))
			b.WriteString(`
  (function() {
    const ctx = document.getElementById('rs_cpu');
    const rates = RS_CPU.map(d => d.TargetKpps);
    const datasets = [];
    const colors = { user: '#4CAF50', system: '#2196F3', irq: '#FF9800', softirq: '#F44336', steal: '#9C27B0', iowait: '#795548' };

    // Find key cores
    const coreNums = Object.keys(KEY_CORES).map(Number);
    if (coreNums.length === 0) {
      // Use aggregate "cpu" if no key cores
      coreNums.push(-1); // sentinel for aggregate
    }

    for (const field of ['user', 'system', 'irq', 'softirq']) {
      for (const coreNum of coreNums) {
        const coreName = coreNum === -1 ? 'cpu' : 'cpu' + coreNum;
        const role = coreNum === -1 ? '' : ' (' + KEY_CORES[coreNum] + ')';
        datasets.push({
          label: coreName + ' ' + field + role,
          data: RS_CPU.map(d => (d.CoreData[coreName] || {})[field] || 0),
          borderColor: colors[field],
          backgroundColor: colors[field] + '80',
          borderWidth: 1,
        });
      }
    }

    new Chart(ctx, {
      type: 'bar',
      data: { labels: rates.map(String), datasets },
      options: {
        responsive: true, animation: false,
        scales: {
          x: { title: { display: true, text: 'Target kpps' }, stacked: false },
          y: { title: { display: true, text: 'CPU %' }, beginAtZero: true, max: 105 },
        },
        plugins: { legend: { position: 'bottom' } }
      }
    });
  })();
`)
		}

		// SoftIRQ vs Load chart
		if len(rateStepSirqs) > 0 {
			sirqJSON, _ := json.Marshal(rateStepSirqs)
			b.WriteString(fmt.Sprintf("  const RS_SIRQ = %s;\n", string(sirqJSON)))
			b.WriteString(`
  (function() {
    const ctx = document.getElementById('rs_softirq');
    const rates = RS_SIRQ.map(d => d.TargetKpps);
    new Chart(ctx, {
      type: 'line',
      data: {
        labels: rates,
        datasets: [
          { label: 'NET_RX events/s', data: RS_SIRQ.map(d => d.NetRX), borderColor: 'rgb(220,40,40)', pointRadius: 4, fill: false },
          { label: 'NET_TX events/s', data: RS_SIRQ.map(d => d.NetTX), borderColor: 'rgb(54,162,235)', pointRadius: 4, fill: false },
        ]
      },
      options: {
        responsive: true, animation: false,
        scales: {
          x: { title: { display: true, text: 'Target kpps' } },
          y: { title: { display: true, text: 'events/s' }, beginAtZero: true },
        }
      }
    });
  })();
`)
		}

		b.WriteString("  </script>\n\n")
	}

	// --- Time-Series Section ---
	b.WriteString("  <div class=\"section-header\" id=\"time-series\">Time-Series Monitoring</div>\n\n")

	for i, sys := range chartData {
		b.WriteString(fmt.Sprintf("  <div class=\"chart-container\">\n    <h2>%s</h2>\n    <div class=\"series-info\">", sys.Name))
		for j, ds := range sys.Datasets {
			if j > 0 {
				b.WriteString(" &middot; ")
			}
			b.WriteString(fmt.Sprintf("%s (%d pts)", ds.Label, len(ds.Data)))
		}
		yLabel := "CPU %"
		if strings.Contains(sys.Name, "Rates") || strings.Contains(sys.Name, "Traffic") {
			yLabel = "events/s"
		}
		if strings.Contains(sys.Name, "Traffic") {
			yLabel = "rate"
		}
		b.WriteString(fmt.Sprintf("</div>\n    <canvas id=\"chart%d\" height=\"110\" data-ylabel=\"%s\"></canvas>\n", chartIdx+i, yLabel))
		b.WriteString("    <button class=\"btn\" onclick=\"resetAllZoom()\">Reset Zoom</button>\n  </div>\n\n")
	}

	// Time-series script
	b.WriteString("  <script>\n")
	b.WriteString("  const TS_DATA = ")
	b.Write(tsJSON)
	b.WriteString(";\n")

	writeChartJSConstants(&b)
	writeTimeSeriesChartLoop(&b, "TS_DATA")

	// --- Analysis Section (placeholder for Claude to fill in) ---
	b.WriteString(`
  </script>

  <div class="section-header" id="analysis-header">Analysis</div>
  <div id="analysis">
    <p class="analysis-placeholder"><!-- ANALYSIS_START -->Analysis pending — run the msu-analyst skill to generate findings.<!-- ANALYSIS_END --></p>
  </div>

  <script>
`)
	writeHTMLEnd(&b)

	return os.WriteFile(outputPath, []byte(b.String()), 0644)
}
