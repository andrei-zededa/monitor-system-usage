package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andrei-zededa/monitor-system-usage/pkg/msuformat"
)

const (
	// timestampFmt 2025_11_21_13_14_04 -> YYYY_MM_DD_HH_MM_SS .
	timestampFmt = "2006_01_02_15_04_05"
)

var (
	version   = "dev" // version string, should be set at build time.
	commit    = ""    // commit id, should be set at build time.
	buildDate = ""
	builtBy   = ""
	treeState = ""
)

type Section struct {
	ID        string
	BeginTime time.Time
	EndTime   time.Time
	Len       time.Duration
}

type CommandSample struct {
	Timestamp time.Time
	Value     float64
	SectionID string
	Lines     []string
}

type CommandAnalysis struct {
	CommandName string
	Samples     []CommandSample
	TotalChange float64
	AvgRate     float64
}

// timeFormats lists the formats accepted by -from / -to flags, tried in order.
var timeFormats = []string{
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006_01_02_15_04_05",
	"15:04:05", // time-only; needs a reference date
}

// parseTimeFlag parses a -from/-to flag value.  refDate supplies the
// year/month/day when the user provides a time-only value like "13:14:04".
func parseTimeFlag(s string, refDate time.Time) (time.Time, error) {
	for _, fmt := range timeFormats {
		t, err := time.Parse(fmt, s)
		if err == nil {
			if fmt == "15:04:05" {
				// Graft the reference date onto the parsed time.
				t = time.Date(refDate.Year(), refDate.Month(), refDate.Day(),
					t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as a timestamp (accepted: 2006-01-02T15:04:05 | 2006-01-02 15:04:05 | 2006_01_02_15_04_05 | 15:04:05)", s)
}

// inTimeRange returns true when t falls within [from, to].
// A zero from or to means "unbounded on that side".
func inTimeRange(t, from, to time.Time) bool {
	if !from.IsZero() && t.Before(from) {
		return false
	}
	if !to.IsZero() && t.After(to) {
		return false
	}
	return true
}

// extractValue extracts a numeric value from the command output lines returning
// true if a value could be found.
func extractValue(lines []string, fieldRe, valueRe *regexp.Regexp) (float64, bool) {
	for _, line := range lines {
		// If a field pattern is specified, only check matching lines.
		if fieldRe != nil && !fieldRe.MatchString(line) {
			continue
		}

		// Extract a numeric value from the line, either the first numeric
		// value from the line in the case of the default value RE of `(\d+\.?\d*)`,
		// or some more specific value.
		if valueRe != nil {
			matches := valueRe.FindStringSubmatch(line)
			if len(matches) == 2 {
				val, err := strconv.ParseFloat(matches[1], 64)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%v", err)
				}
				return val, true
			}
		}
	}

	return 0, false
}

// analyzeChangingCommands analyzes all collected command samples and returns
// only those commands that had changing values, sorted by absolute total change.
func analyzeChangingCommands(allSamples map[string][]CommandSample) []CommandAnalysis {
	var results []CommandAnalysis

	for cmdName, samples := range allSamples {
		if len(samples) < 2 {
			continue
		}

		analysis := CommandAnalysis{
			CommandName: cmdName,
			Samples:     samples,
		}

		// Check for changes and calculate totals
		hasChanges := false
		for i := 1; i < len(samples); i++ {
			delta := samples[i].Value - samples[i-1].Value
			if delta != 0 {
				hasChanges = true
			}
		}

		if !hasChanges {
			continue
		}

		// Calculate total change and average rate
		analysis.TotalChange = samples[len(samples)-1].Value - samples[0].Value
		timeSpan := samples[len(samples)-1].Timestamp.Sub(samples[0].Timestamp).Seconds()
		if timeSpan > 0 {
			analysis.AvgRate = analysis.TotalChange / timeSpan
		}

		results = append(results, analysis)
	}

	// Sort by absolute total change (descending)
	sort.Slice(results, func(i, j int) bool {
		return math.Abs(results[i].TotalChange) > math.Abs(results[j].TotalChange)
	})

	return results
}

// parseOpts carries the flags needed by the file parsers.
type parseOpts struct {
	trackCommand    string
	changingPattern string
	sectionType     string
	fieldRe         *regexp.Regexp
	valueRe         *regexp.Regexp
}

// parseResult holds the data extracted from a file (text or CBOR).
type parseResult struct {
	aSections      map[string]*Section
	aSectionsOrder []string
	bSections      map[string]*Section
	bSectionsOrder []string

	// -command mode
	commandSamples []CommandSample

	// -changing mode
	allCommandSamples map[string][]CommandSample
	matchingCmdCount  int
}

// parseTextFile parses a legacy text msu.out file.
func parseTextFile(filename string, opts parseOpts) (*parseResult, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	aBeginRe := regexp.MustCompile(`^\+\+\+\+ BEGIN\s+(\d+)\s+(\S+)`)
	aEndRe := regexp.MustCompile(`^\+\+\+\+ END\s+(\d+)\s+(\S+)`)
	bBeginRe := regexp.MustCompile(`^==== BEGIN\s+(\d+)\s+(\S+)`)
	bEndRe := regexp.MustCompile(`^==== END\s+(\d+)\s+(\S+)`)
	cmdMarkerRe := regexp.MustCompile(`^-+>\s*(.+)$`)

	res := &parseResult{
		aSections:         make(map[string]*Section),
		bSections:         make(map[string]*Section),
		allCommandSamples: make(map[string][]CommandSample),
	}

	inASection := false
	inBSection := false

	var currentSectionID string
	var currentSectionTime time.Time
	var inTargetCommand bool
	var commandOutputLines []string

	currentTrackingCmds := make(map[string][]string)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		if matches := aBeginRe.FindStringSubmatch(line); matches != nil {
			id := matches[1]
			ts := matches[2]
			if inASection || inBSection {
				continue
			}
			beginTime, err := time.Parse(timestampFmt, ts)
			if err != nil {
				continue
			}
			res.aSections[id] = &Section{ID: id, BeginTime: beginTime}
			res.aSectionsOrder = append(res.aSectionsOrder, id)
			inASection = true
			currentSectionID = id
			currentSectionTime = beginTime
			continue
		}

		if matches := aEndRe.FindStringSubmatch(line); matches != nil {
			id := matches[1]
			ts := matches[2]
			if endTime, err := time.Parse(timestampFmt, ts); err == nil {
				if s, exists := res.aSections[id]; exists {
					s.EndTime = endTime
				}
			}
			// Flush any pending command output before leaving the section.
			if opts.trackCommand != "" && inTargetCommand {
				if value, ok := extractValue(commandOutputLines, opts.fieldRe, opts.valueRe); ok {
					if shouldTrackSection(opts.sectionType, inASection, inBSection) {
						res.commandSamples = append(res.commandSamples, CommandSample{
							Timestamp: currentSectionTime, Value: value,
							SectionID: currentSectionID, Lines: commandOutputLines,
						})
					}
				}
				commandOutputLines = nil
				inTargetCommand = false
			}
			if opts.changingPattern != "" {
				flushChangingCmds(currentTrackingCmds, opts, currentSectionTime, currentSectionID, inASection, inBSection, res)
				currentTrackingCmds = make(map[string][]string)
			}
			inASection = false
			continue
		}

		if matches := bBeginRe.FindStringSubmatch(line); matches != nil {
			id := matches[1]
			ts := matches[2]
			if inASection || inBSection {
				continue
			}
			beginTime, err := time.Parse(timestampFmt, ts)
			if err != nil {
				continue
			}
			res.bSections[id] = &Section{ID: id, BeginTime: beginTime}
			res.bSectionsOrder = append(res.bSectionsOrder, id)
			inBSection = true
			currentSectionID = id
			currentSectionTime = beginTime
			continue
		}

		if matches := bEndRe.FindStringSubmatch(line); matches != nil {
			id := matches[1]
			ts := matches[2]
			if endTime, err := time.Parse(timestampFmt, ts); err == nil {
				if s, exists := res.bSections[id]; exists {
					s.EndTime = endTime
				}
			}
			if opts.trackCommand != "" && inTargetCommand {
				if value, ok := extractValue(commandOutputLines, opts.fieldRe, opts.valueRe); ok {
					if shouldTrackSection(opts.sectionType, inASection, inBSection) {
						res.commandSamples = append(res.commandSamples, CommandSample{
							Timestamp: currentSectionTime, Value: value,
							SectionID: currentSectionID, Lines: commandOutputLines,
						})
					}
				}
				commandOutputLines = nil
				inTargetCommand = false
			}
			if opts.changingPattern != "" {
				flushChangingCmds(currentTrackingCmds, opts, currentSectionTime, currentSectionID, inASection, inBSection, res)
				currentTrackingCmds = make(map[string][]string)
			}
			inBSection = false
			continue
		}

		// Command tracking for -command mode.
		if opts.trackCommand != "" && (inASection || inBSection) {
			if matches := cmdMarkerRe.FindStringSubmatch(line); matches != nil {
				cmdName := strings.TrimSpace(matches[1])
				if inTargetCommand {
					if value, ok := extractValue(commandOutputLines, opts.fieldRe, opts.valueRe); ok {
						if shouldTrackSection(opts.sectionType, inASection, inBSection) {
							res.commandSamples = append(res.commandSamples, CommandSample{
								Timestamp: currentSectionTime, Value: value,
								SectionID: currentSectionID, Lines: commandOutputLines,
							})
						}
					}
					commandOutputLines = nil
					inTargetCommand = false
				}
				if cmdName == opts.trackCommand {
					inTargetCommand = true
					commandOutputLines = []string{}
				}
				continue
			}
			if inTargetCommand {
				commandOutputLines = append(commandOutputLines, line)
			}
		}

		// Multi-command tracking for -changing mode.
		if opts.changingPattern != "" && (inASection || inBSection) {
			if matches := cmdMarkerRe.FindStringSubmatch(line); matches != nil {
				cmdName := strings.TrimSpace(matches[1])
				flushChangingCmds(currentTrackingCmds, opts, currentSectionTime, currentSectionID, inASection, inBSection, res)
				currentTrackingCmds = make(map[string][]string)
				if strings.Contains(cmdName, opts.changingPattern) {
					currentTrackingCmds[cmdName] = []string{}
					if _, seen := res.allCommandSamples[cmdName]; !seen {
						res.allCommandSamples[cmdName] = []CommandSample{}
						res.matchingCmdCount++
					}
				}
				continue
			}
			for cmdName := range currentTrackingCmds {
				currentTrackingCmds[cmdName] = append(currentTrackingCmds[cmdName], line)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// shouldTrackSection returns true if the current section type matches the filter.
func shouldTrackSection(sectionType string, inA, inB bool) bool {
	switch sectionType {
	case "A":
		return inA
	case "B":
		return inB
	default:
		return true
	}
}

// flushChangingCmds processes accumulated command output in -changing mode.
func flushChangingCmds(tracking map[string][]string, opts parseOpts, ts time.Time, secID string, inA, inB bool, res *parseResult) {
	for cmd, lines := range tracking {
		if value, ok := extractValue(lines, opts.fieldRe, opts.valueRe); ok {
			if shouldTrackSection(opts.sectionType, inA, inB) {
				res.allCommandSamples[cmd] = append(res.allCommandSamples[cmd], CommandSample{
					Timestamp: ts, Value: value, SectionID: secID, Lines: lines,
				})
			}
		}
	}
}

// parseCBORFile parses an MSU CBOR file into the same parseResult as parseTextFile.
func parseCBORFile(filename string, opts parseOpts) (*parseResult, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := msuformat.NewReader(f)
	if _, err := r.ReadHeader(); err != nil {
		return nil, fmt.Errorf("reading CBOR header: %w", err)
	}

	res := &parseResult{
		aSections:         make(map[string]*Section),
		bSections:         make(map[string]*Section),
		allCommandSamples: make(map[string][]CommandSample),
	}

	// Track section ordering and time ranges.
	type sectionKey struct {
		seq     int64
		secType string
	}
	sectionSeen := make(map[sectionKey]bool)

	for {
		sample, err := r.Next()
		if err != nil {
			return nil, fmt.Errorf("reading CBOR sample: %w", err)
		}
		if sample == nil {
			break
		}
		if sample.Section == "init" {
			continue
		}

		ts, err := sample.ParseTime()
		if err != nil {
			continue
		}

		id := strconv.FormatInt(sample.Seq, 10)
		key := sectionKey{sample.Seq, sample.Section}

		// Build section maps.
		switch sample.Section {
		case "A":
			if !sectionSeen[key] {
				sectionSeen[key] = true
				res.aSections[id] = &Section{ID: id, BeginTime: ts}
				res.aSectionsOrder = append(res.aSectionsOrder, id)
			}
			// Update end time to the latest sample in this section.
			if s := res.aSections[id]; s != nil {
				s.EndTime = ts
			}
		case "B":
			if !sectionSeen[key] {
				sectionSeen[key] = true
				res.bSections[id] = &Section{ID: id, BeginTime: ts}
				res.bSectionsOrder = append(res.bSectionsOrder, id)
			}
			if s := res.bSections[id]; s != nil {
				s.EndTime = ts
			}
		}

		lines := strings.Split(sample.Out, "\n")
		inA := sample.Section == "A"
		inB := sample.Section == "B"

		// -command mode
		if opts.trackCommand != "" && sample.Cmd == opts.trackCommand {
			if shouldTrackSection(opts.sectionType, inA, inB) {
				if value, ok := extractValue(lines, opts.fieldRe, opts.valueRe); ok {
					res.commandSamples = append(res.commandSamples, CommandSample{
						Timestamp: ts, Value: value, SectionID: id, Lines: lines,
					})
				} else if strings.Contains(opts.trackCommand, "/proc/stat") {
					// For /proc/stat we don't extract a single value; collect lines.
					res.commandSamples = append(res.commandSamples, CommandSample{
						Timestamp: ts, SectionID: id, Lines: lines,
					})
				}
			}
		}

		// -changing mode
		if opts.changingPattern != "" && strings.Contains(sample.Cmd, opts.changingPattern) {
			if _, seen := res.allCommandSamples[sample.Cmd]; !seen {
				res.allCommandSamples[sample.Cmd] = []CommandSample{}
				res.matchingCmdCount++
			}
			if shouldTrackSection(opts.sectionType, inA, inB) {
				if value, ok := extractValue(lines, opts.fieldRe, opts.valueRe); ok {
					res.allCommandSamples[sample.Cmd] = append(res.allCommandSamples[sample.Cmd], CommandSample{
						Timestamp: ts, Value: value, SectionID: id, Lines: lines,
					})
				}
			}
		}
	}

	return res, nil
}

// parseFile auto-detects the file format and dispatches to the appropriate parser.
func parseFile(filename string, opts parseOpts) (*parseResult, error) {
	if isCBORFile(filename) {
		return parseCBORFile(filename, opts)
	}
	return parseTextFile(filename, opts)
}

func main() {
	// Define command-line flags
	var (
		trackCommand    = flag.String("command", "", "Command to track (e.g., 'cat /proc/net/dev')")
		sectionType     = flag.String("section-type", "both", "Section type to search: A, B, or both")
		fieldPattern    = flag.String("field-pattern", "", "Regex pattern to identify the line with the value")
		valuePattern    = flag.String("value-pattern", `(\d+\.?\d*)`, "Regex pattern to extract numeric value")
		showASections   = flag.Bool("show-a-sections", true, "Show A sections analysis")
		showBSections   = flag.Bool("show-b-sections", true, "Show B sections analysis")
		fromFlag        = flag.String("from", "", "Show only data at or after this time (e.g. 13:14:04, 2025-01-21T13:14:04)")
		toFlag          = flag.String("to", "", "Show only data at or before this time (e.g. 13:15:00, 2025-01-21T13:15:00)")
		changingPattern = flag.String("changing", "", "Show commands with changing values matching this substring pattern")
		cpuCompare      = flag.String("cpu-compare", "", "Directory with CPU data files; generates an HTML comparison chart")
		reportDir       = flag.String("report", "", "Generate unified HTML report from MSU data directory")
		reportIperf     = flag.String("iperf-json", "", "iperf JSON (from parse_iperf.py) for rate-step charts in -report")
		reportParams    = flag.String("params-json", "", "test params JSON (from extract_test_params.py) for -report")
		showVer         = flag.Bool("version", false, "Show version and exit")
	)

	flag.Parse()

	if *showVer {
		fmt.Printf("msu version %s\n", version)
		return
	}

	// CPU comparison mode: process directory and exit
	if *cpuCompare != "" {
		if err := runCPUCompare(*cpuCompare); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Report mode: generate unified HTML report
	if *reportDir != "" {
		if err := runReport(*reportDir, *reportIperf, *reportParams); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Validate that -iperf-json and -params-json are only used with -report
	if *reportIperf != "" || *reportParams != "" {
		fmt.Fprintf(os.Stderr, "Error: -iperf-json and -params-json require -report\n")
		os.Exit(1)
	}

	// -command and -changing are mutually exclusive
	if *trackCommand != "" && *changingPattern != "" {
		fmt.Fprintf(os.Stderr, "Error: -command and -changing are mutually exclusive\n")
		os.Exit(1)
	}

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <logfile>\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	filename := flag.Arg(0)

	var fieldRe *regexp.Regexp
	var valueRe *regexp.Regexp
	if *fieldPattern != "" {
		fieldRe = regexp.MustCompile(*fieldPattern)
	}
	if *valuePattern != "" {
		valueRe = regexp.MustCompile(*valuePattern)
	}

	opts := parseOpts{
		trackCommand:    *trackCommand,
		changingPattern: *changingPattern,
		sectionType:     *sectionType,
		fieldRe:         fieldRe,
		valueRe:         valueRe,
	}

	res, err := parseFile(filename, opts)
	if err != nil {
		log.Fatal(err)
	}

	aSections := res.aSections
	aSectionsOrder := res.aSectionsOrder
	bSections := res.bSections
	bSectionsOrder := res.bSectionsOrder
	commandSamples := res.commandSamples
	allCommandSamples := res.allCommandSamples
	matchingCmdCount := res.matchingCmdCount

	// --- Parse -from / -to time range flags ---
	var fromTime, toTime time.Time

	if *fromFlag != "" || *toFlag != "" {
		// Find a reference date from the earliest section for time-only parsing.
		var refDate time.Time
		if len(aSectionsOrder) > 0 {
			refDate = aSections[aSectionsOrder[0]].BeginTime
		} else if len(bSectionsOrder) > 0 {
			refDate = bSections[bSectionsOrder[0]].BeginTime
		}

		if *fromFlag != "" {
			fromTime, err = parseTimeFlag(*fromFlag, refDate)
			if err != nil {
				log.Fatal(err)
			}
		}
		if *toFlag != "" {
			toTime, err = parseTimeFlag(*toFlag, refDate)
			if err != nil {
				log.Fatal(err)
			}
		}
	}

	// Filter command samples to the requested time range.
	if !fromTime.IsZero() || !toTime.IsZero() {
		filtered := commandSamples[:0]
		for _, s := range commandSamples {
			if inTimeRange(s.Timestamp, fromTime, toTime) {
				filtered = append(filtered, s)
			}
		}
		commandSamples = filtered
	}

	// Filter multi-command samples to the requested time range.
	if !fromTime.IsZero() || !toTime.IsZero() {
		for cmdName, samples := range allCommandSamples {
			filtered := samples[:0]
			for _, s := range samples {
				if inTimeRange(s.Timestamp, fromTime, toTime) {
					filtered = append(filtered, s)
				}
			}
			allCommandSamples[cmdName] = filtered
		}
	}

	// Handle -changing mode output first (suppresses section output)
	if *changingPattern != "" {
		results := analyzeChangingCommands(allCommandSamples)

		fmt.Printf("\nCommands with changing values (pattern: %q)\n", *changingPattern)
		fmt.Println("================================================================================")
		fmt.Printf("%-60s %8s %14s %12s\n", "COMMAND", "SAMPLES", "TOTAL_CHANGE", "RATE/s")
		fmt.Println("--------------------------------------------------------------------------------")

		for _, r := range results {
			cmdDisplay := r.CommandName
			if len(cmdDisplay) > 60 {
				cmdDisplay = cmdDisplay[:57] + "..."
			}
			fmt.Printf("%-60s %8d %14.2f %12.2f\n",
				cmdDisplay,
				len(r.Samples),
				r.TotalChange,
				r.AvgRate)
		}

		fmt.Println("================================================================================")
		fmt.Printf("Found %d commands with changing values out of %d matching pattern\n",
			len(results), matchingCmdCount)

		return
	}

	if *showASections {
		fmt.Println("\"A\" sections analysis")
		fmt.Println("=======================")
		fmt.Printf("%-4s %-10s %-20s %-20s %-15s\n", "No", "ID", "Begin Time", "End Time", "Duration")
		fmt.Println("--------------------------------------------------------------------------------")

		shown := 0
		for i, sID := range aSectionsOrder {
			s := aSections[sID]

			if !inTimeRange(s.BeginTime, fromTime, toTime) {
				continue
			}

			// Check if we have both begin and end times.
			if s.EndTime.IsZero() {
				fmt.Printf("%-4d %-10s %-20s %-20s %-15s\n",
					i, sID,
					s.BeginTime.Format("2006-01-02 15:04:05"),
					"(no end marker)",
					"N/A")
				shown++
				continue
			}

			if s.EndTime.Equal(s.BeginTime) {
				if i < len(aSectionsOrder)-1 {
					nextS := aSections[aSectionsOrder[i+1]]
					duration := nextS.BeginTime.Sub(s.BeginTime)
					fmt.Printf("%-4d %-10s %-20s %-20s ~%-15s\n",
						i, sID,
						s.BeginTime.Format("2006-01-02 15:04:05"),
						s.EndTime.Format("2006-01-02 15:04:05"),
						duration.String())
					shown++
					continue
				}
			}

			duration := s.EndTime.Sub(s.BeginTime)
			fmt.Printf("%-4d %-10s %-20s %-20s %-15s\n",
				i, sID,
				s.BeginTime.Format("2006-01-02 15:04:05"),
				s.EndTime.Format("2006-01-02 15:04:05"),
				duration.String())
			shown++
		}

		fmt.Println("=======================")
		fmt.Printf("Total \"A\" sections found: %d (shown: %d)\n", len(aSectionsOrder), shown)

		fmt.Println()
	}

	if *showBSections {
		fmt.Println("\"B\" sections analysis")
		fmt.Println("=======================")
		fmt.Printf("%-4s %-10s %-20s %-20s %-15s\n", "No", "ID", "Begin Time", "End Time", "Duration")
		fmt.Println("--------------------------------------------------------------------------------")

		shown := 0
		for i, sID := range bSectionsOrder {
			s := bSections[sID]

			if !inTimeRange(s.BeginTime, fromTime, toTime) {
				continue
			}

			// Check if we have both begin and end times.
			if s.EndTime.IsZero() {
				fmt.Printf("%-4d %-10s %-20s %-20s %-15s\n",
					i, sID,
					s.BeginTime.Format("2006-01-02 15:04:05"),
					"(no end marker)",
					"N/A")
				shown++
				continue
			}

			if s.EndTime.Equal(s.BeginTime) {
				if i < len(bSectionsOrder)-1 {
					nextS := bSections[bSectionsOrder[i+1]]
					duration := nextS.BeginTime.Sub(s.BeginTime)
					fmt.Printf("%-4d %-10s %-20s %-20s ~%-15s\n",
						i, sID,
						s.BeginTime.Format("2006-01-02 15:04:05"),
						s.EndTime.Format("2006-01-02 15:04:05"),
						duration.String())
					shown++
					continue
				}
			}

			duration := s.EndTime.Sub(s.BeginTime)
			fmt.Printf("%-4d %-10s %-20s %-20s %-15s\n",
				i, sID,
				s.BeginTime.Format("2006-01-02 15:04:05"),
				s.EndTime.Format("2006-01-02 15:04:05"),
				duration.String())
			shown++
		}

		fmt.Println("=======================")
		fmt.Printf("Total \"B\" sections found: %d (shown: %d)\n", len(bSectionsOrder), shown)
	}

	// Command tracking analysis.
	if *trackCommand != "" && strings.Contains(*trackCommand, "/proc/stat") && len(commandSamples) > 1 {
		for i := 1; i < len(commandSamples); i++ {
			curr := commandSamples[i]
			prev := commandSamples[i-1]
			fmt.Printf("%-4d %-10s %-20s    ",
				i+1,
				curr.SectionID,
				curr.Timestamp.Format("2006-01-02 15:04:05"))

			PrintCPUAndSoftIRQStats(prev.Lines, curr.Lines, prev.Timestamp, curr.Timestamp, 100)
		}

		return
	}

	if *trackCommand != "" && len(commandSamples) > 0 {
		fmt.Println()
		fmt.Println("Command value tracking analysis")
		fmt.Println("===============================")
		fmt.Printf("Command: %s\n", *trackCommand)
		fmt.Printf("Section type(s): %s\n", *sectionType)
		if *fieldPattern != "" {
			fmt.Printf("Field pattern: %s\n", *fieldPattern)
		}
		fmt.Printf("Value pattern: %s\n", *valuePattern)
		fmt.Printf("Samples found: %d\n", len(commandSamples))
		fmt.Println()

		// Calculate statistics
		firstSample := commandSamples[0]
		lastSample := commandSamples[len(commandSamples)-1]

		totalChange := lastSample.Value - firstSample.Value
		timeElapsed := lastSample.Timestamp.Sub(firstSample.Timestamp)

		fmt.Printf("First value: %.2f (section %s at %s)\n",
			firstSample.Value,
			firstSample.SectionID,
			firstSample.Timestamp.Format("2006-01-02 15:04:05"))

		fmt.Printf("Last value:  %.2f (section %s at %s)\n",
			lastSample.Value,
			lastSample.SectionID,
			lastSample.Timestamp.Format("2006-01-02 15:04:05"))

		fmt.Printf("\nTotal change: %.2f\n", totalChange)
		fmt.Printf("Time elapsed: %s\n", timeElapsed)

		if timeElapsed.Seconds() > 0 {
			avgSpeed := totalChange / timeElapsed.Seconds()
			fmt.Printf("Average speed of change: %.2f/s\n", avgSpeed)
			/*
				fmt.Printf("                        %.4f per minute\n", avgSpeed*60)
				fmt.Printf("                        %.4f per hour\n", avgSpeed*3600)
			*/
		}

		// Show all samples
		fmt.Println("\nAll samples:")
		fmt.Printf("%-4s %-10s %-20s %-18s %s\n", "No", "Section", "Timestamp", "Value", "Change")
		fmt.Println("--------------------------------------------------------------------------------")
		for i, sample := range commandSamples {
			avgSpeed := float64(0)
			if i > 0 {
				prev := commandSamples[i-1]
				avgSpeed = (sample.Value - prev.Value) / sample.Timestamp.Sub(prev.Timestamp).Seconds()
			}
			fmt.Printf("%-4d %-10s %-20s %-18.2f %.2f/s\n",
				i+1,
				sample.SectionID,
				sample.Timestamp.Format("2006-01-02 15:04:05"),
				sample.Value,
				avgSpeed)
		}
		fmt.Println("===============================")

		return
	}

	if *trackCommand != "" && len(commandSamples) == 0 {
		fmt.Println()
		fmt.Println("Command value tracking analysis")
		fmt.Println("===============================")
		fmt.Printf("Command: %s\n", *trackCommand)
		fmt.Printf("No samples found for the specified command.\n")
		fmt.Println("===============================")

		return
	}
}
