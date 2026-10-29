package main

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andrei-zededa/monitor-system-usage/pkg/msuformat"
)

// ICMP ping monitor analysis.
//
// msu-collect -ping <ip> runs a background ICMP echo monitor per target and
// snapshots its cumulative counters into every B section as a sample whose
// cmd is "ping <ip>" (see pkg/msucollect/ping.go). This file turns those
// samples into per-interval deltas (requests sent, replies, losses, RTT) and
// produces the text report behind `msu -ping` plus the series for the
// "ICMP Ping Loss & RTT" chart of `msu -report`.

// pingCmdPrefix is the cmd prefix of the ping monitor samples.
const pingCmdPrefix = "ping "

// pingGapFactor flags a sampling interval as a gap when it is longer than
// this many times the collector's configured interval.
const pingGapFactor = 2.5

// pingStarvedFraction flags an interval as starved when fewer than this
// fraction of the expected echo requests were sent in it.
const pingStarvedFraction = 0.8

// pingCounters mirrors the "key value" lines of one ping sample. Every
// counter is cumulative since the monitor started, except RTTMinUS/RTTMaxUS
// (replies since the previous sample) and Pending (instantaneous).
type pingCounters struct {
	Target     string
	Mode       string
	ID         uint64
	IntervalUS uint64
	TimeoutUS  uint64
	TX         uint64
	TXErr      uint64
	RX         uint64
	RXErr      uint64
	Lost       uint64
	Late       uint64
	Dup        uint64
	Stray      uint64
	Pending    uint64
	RTTSumUS   uint64
	RTTSumSqUS uint64
	RTTLastUS  uint64
	RTTMinUS   uint64
	RTTMaxUS   uint64
}

// parsePingCounters parses the output lines of a "ping <ip>" sample. It
// returns false when the lines do not look like a ping sample (no "tx"
// line).
func parsePingCounters(lines []string) (pingCounters, bool) {
	var c pingCounters
	seenTX := false
	for _, line := range lines {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "target":
			c.Target = val
			continue
		case "mode":
			c.Mode = val
			continue
		}
		n, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "id":
			c.ID = n
		case "interval_us":
			c.IntervalUS = n
		case "timeout_us":
			c.TimeoutUS = n
		case "tx":
			c.TX = n
			seenTX = true
		case "tx_err":
			c.TXErr = n
		case "rx":
			c.RX = n
		case "rx_err":
			c.RXErr = n
		case "lost":
			c.Lost = n
		case "late":
			c.Late = n
		case "dup":
			c.Dup = n
		case "stray":
			c.Stray = n
		case "pending":
			c.Pending = n
		case "rtt_sum_us":
			c.RTTSumUS = n
		case "rtt_sumsq_us":
			c.RTTSumSqUS = n
		case "rtt_last_us":
			c.RTTLastUS = n
		case "rtt_min_us":
			c.RTTMinUS = n
		case "rtt_max_us":
			c.RTTMaxUS = n
		}
	}
	return c, seenTX
}

// pingSample is one "ping <ip>" sample with its parsed counters.
type pingSample struct {
	Time time.Time
	Seq  int64
	Run  int // index into pingFile.Runs of the collector run this sample belongs to
	Err  string
	C    pingCounters
}

// pingRun describes one msu-collect invocation found in the file. A file
// holds several runs when the collector was restarted with the same output
// path (the writer appends, each run starts with a new header).
type pingRun struct {
	Start    time.Time
	Interval time.Duration // collector sampling interval
	MsuVer   string
	Hostname string
	Kernel   string
	CmdLine  []string
}

// pingFile is everything the ping analysis needs from one CBOR file.
type pingFile struct {
	Runs    []pingRun
	Targets []string                // sorted numerically by IPv4 address
	Samples map[string][]pingSample // keyed by target, in file order
}

// collectPingSamples reads every "ping <ip>" sample from a CBOR file.
func collectPingSamples(filename string) (*pingFile, error) {
	if !isCBORFile(filename) {
		return nil, fmt.Errorf("%s: ping monitor data is only available in CBOR files", filename)
	}
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := msuformat.NewReader(f)
	h, err := r.ReadHeader()
	if err != nil {
		return nil, fmt.Errorf("reading CBOR header: %w", err)
	}

	pf := &pingFile{Samples: make(map[string][]pingSample)}
	addRun := func(h *msuformat.Header) {
		pf.Runs = append(pf.Runs, pingRun{
			Start:    time.Unix(0, h.TS).UTC(),
			Interval: time.Duration(h.IntervalNS),
			MsuVer:   h.MsuVer,
			Hostname: h.Hostname,
			Kernel:   h.KernelRelease,
			CmdLine:  h.CmdLine,
		})
	}
	addRun(h)
	r.OnHeader = addRun

	for {
		s, err := r.Next()
		if err != nil {
			return nil, fmt.Errorf("reading CBOR sample: %w", err)
		}
		if s == nil {
			break
		}
		if !strings.HasPrefix(s.Cmd, pingCmdPrefix) {
			continue
		}
		c, ok := parsePingCounters(strings.Split(s.Out, "\n"))
		if !ok && s.Err == "" {
			continue
		}
		target := strings.TrimPrefix(s.Cmd, pingCmdPrefix)
		if _, seen := pf.Samples[target]; !seen {
			pf.Targets = append(pf.Targets, target)
		}
		pf.Samples[target] = append(pf.Samples[target], pingSample{
			Time: s.ParseTime(),
			Seq:  s.Seq,
			Run:  len(pf.Runs) - 1,
			Err:  s.Err,
			C:    c,
		})
	}
	sortPingTargets(pf.Targets)
	return pf, nil
}

// sortPingTargets sorts IPv4 targets numerically; anything that does not
// parse as IPv4 sorts after them, lexically.
func sortPingTargets(targets []string) {
	sort.SliceStable(targets, func(i, j int) bool {
		a, b := net.ParseIP(targets[i]).To4(), net.ParseIP(targets[j]).To4()
		switch {
		case a != nil && b != nil:
			return bytes.Compare(a, b) < 0
		case a != nil:
			return true
		case b != nil:
			return false
		}
		return targets[i] < targets[j]
	})
}

// pingInterval holds the counter deltas between two consecutive samples of
// one target, i.e. what happened to the monitor in that sampling interval.
type pingInterval struct {
	Start time.Time // previous sample (or collector run start for the first sample of a run)
	End   time.Time // this sample
	Seq   int64

	TX, TXErr, RX, RXErr   uint64
	Lost, Late, Dup, Stray uint64
	Pending                uint64 // in flight at End (instantaneous)
	RTTSumUS, RTTSumSqUS   uint64
	RTTMinUS, RTTMaxUS     uint64 // 0 when no reply arrived in the interval
	Err                    string

	NewRun  bool // first interval of a collector run; counters started from zero
	Gap     bool // interval much longer than the collector's sampling interval
	Starved bool // far fewer echo requests sent than the ping interval allows
}

// Duration returns the length of the interval.
func (iv pingInterval) Duration() time.Duration { return iv.End.Sub(iv.Start) }

// LossPct returns the share of requests of this interval that were lost
// (including those answered late). Losses are counted when the timeout
// expires, so a loss can belong to a request sent at the very end of the
// previous interval; the result is capped at 100.
func (iv pingInterval) LossPct() float64 {
	if iv.Lost == 0 {
		return 0
	}
	if iv.TX == 0 {
		return 100
	}
	return math.Min(100, float64(iv.Lost)*100/float64(iv.TX))
}

// RTTAvgUS returns the mean RTT of the in-time replies of this interval,
// or 0 when there were none.
func (iv pingInterval) RTTAvgUS() float64 {
	if iv.RX == 0 {
		return 0
	}
	return float64(iv.RTTSumUS) / float64(iv.RX)
}

// pingIntervals turns the samples of one target into per-interval deltas.
// Samples must be in file order. The first sample of every collector run
// is diffed against zero and its interval starts at the run start, so the
// init phase (during which the monitors already run) is covered too.
func pingIntervals(samples []pingSample, runs []pingRun) []pingInterval {
	ivals := make([]pingInterval, 0, len(samples))
	for i, cur := range samples {
		var prev pingCounters
		var start time.Time
		newRun := i == 0 || samples[i-1].Run != cur.Run || pingCountersRestarted(samples[i-1].C, cur.C)
		if newRun {
			if cur.Run >= 0 && cur.Run < len(runs) && !runs[cur.Run].Start.IsZero() && runs[cur.Run].Start.Before(cur.Time) {
				start = runs[cur.Run].Start
			} else {
				start = cur.Time
			}
		} else {
			prev = samples[i-1].C
			start = samples[i-1].Time
		}
		c := cur.C
		iv := pingInterval{
			Start:      start,
			End:        cur.Time,
			Seq:        cur.Seq,
			TX:         safeU64Delta(c.TX, prev.TX),
			TXErr:      safeU64Delta(c.TXErr, prev.TXErr),
			RX:         safeU64Delta(c.RX, prev.RX),
			RXErr:      safeU64Delta(c.RXErr, prev.RXErr),
			Lost:       safeU64Delta(c.Lost, prev.Lost),
			Late:       safeU64Delta(c.Late, prev.Late),
			Dup:        safeU64Delta(c.Dup, prev.Dup),
			Stray:      safeU64Delta(c.Stray, prev.Stray),
			Pending:    c.Pending,
			RTTSumUS:   safeU64Delta(c.RTTSumUS, prev.RTTSumUS),
			RTTSumSqUS: safeU64Delta(c.RTTSumSqUS, prev.RTTSumSqUS),
			RTTMinUS:   c.RTTMinUS,
			RTTMaxUS:   c.RTTMaxUS,
			Err:        cur.Err,
			NewRun:     newRun,
		}
		dur := iv.Duration()
		if !newRun && dur > 0 {
			if cur.Run >= 0 && cur.Run < len(runs) && runs[cur.Run].Interval > 0 &&
				float64(dur) > pingGapFactor*float64(runs[cur.Run].Interval) {
				iv.Gap = true
			}
			if c.IntervalUS > 0 && c.Mode != "none" {
				expected := float64(dur/time.Microsecond) / float64(c.IntervalUS)
				if float64(iv.TX) < pingStarvedFraction*expected {
					iv.Starved = true
				}
			}
		}
		ivals = append(ivals, iv)
	}
	return ivals
}

// pingCountersRestarted reports whether the cumulative counters went
// backwards, which means the monitor (collector) was restarted.
func pingCountersRestarted(prev, cur pingCounters) bool {
	return cur.TX < prev.TX || cur.Lost < prev.Lost || cur.RX < prev.RX
}

// pingEpisode is a run of consecutive intervals that all saw loss.
type pingEpisode struct {
	Start, End time.Time
	Intervals  int
	TX, Lost   uint64
	Late       uint64
	RTTMaxUS   uint64
}

// LossPct returns the loss share over the whole episode.
func (e pingEpisode) LossPct() float64 {
	if e.Lost == 0 {
		return 0
	}
	if e.TX == 0 {
		return 100
	}
	return math.Min(100, float64(e.Lost)*100/float64(e.TX))
}

// pingError is a distinct error string seen in a target's samples.
type pingError struct {
	Msg   string
	Count int
	First time.Time
}

// pingTargetReport summarizes one target over the selected intervals.
type pingTargetReport struct {
	Target     string
	Mode       string
	ID         uint64
	IntervalUS uint64
	TimeoutUS  uint64

	First, Last time.Time // start of the first interval (run start for an unfiltered report) and last sample
	Samples     int
	Intervals   []pingInterval

	TX, TXErr, RX, RXErr   uint64
	Lost, Late, Dup, Stray uint64

	RTTMeanUS, RTTStddevUS float64
	RTTMinUS, RTTMaxUS     uint64

	LossyIntervals int
	Episodes       []pingEpisode
	Gaps           int
	Starved        int
	MaxInterval    time.Duration
	Errors         []pingError
}

// LossPct returns the overall loss share (lost / sent).
func (r *pingTargetReport) LossPct() float64 {
	if r.Lost == 0 {
		return 0
	}
	if r.TX == 0 {
		return 100
	}
	return math.Min(100, float64(r.Lost)*100/float64(r.TX))
}

// buildPingReport computes the per-target report. When from/to are
// non-zero only intervals ending inside [from, to] are considered.
func buildPingReport(target string, samples []pingSample, runs []pingRun, from, to time.Time) *pingTargetReport {
	r := &pingTargetReport{Target: target}
	if len(samples) == 0 {
		return r
	}
	all := pingIntervals(samples, runs)
	var kept []pingInterval
	for _, iv := range all {
		if inTimeRange(iv.End, from, to) {
			kept = append(kept, iv)
		}
	}
	r.Intervals = kept
	r.Samples = len(kept)
	if len(kept) == 0 {
		return r
	}
	r.First = kept[0].Start
	r.Last = kept[len(kept)-1].End

	// Socket details from the last sample that had a socket.
	for i := len(samples) - 1; i >= 0; i-- {
		c := samples[i].C
		if c.Mode != "" && c.Mode != "none" {
			r.Mode, r.ID = c.Mode, c.ID
			r.IntervalUS, r.TimeoutUS = c.IntervalUS, c.TimeoutUS
			break
		}
	}
	if r.IntervalUS == 0 {
		c := samples[len(samples)-1].C
		r.Mode, r.ID, r.IntervalUS, r.TimeoutUS = c.Mode, c.ID, c.IntervalUS, c.TimeoutUS
	}

	var rttSum, rttSumSq uint64
	errIdx := make(map[string]int)
	var ep *pingEpisode
	for _, iv := range kept {
		r.TX += iv.TX
		r.TXErr += iv.TXErr
		r.RX += iv.RX
		r.RXErr += iv.RXErr
		r.Lost += iv.Lost
		r.Late += iv.Late
		r.Dup += iv.Dup
		r.Stray += iv.Stray
		rttSum += iv.RTTSumUS
		rttSumSq += iv.RTTSumSqUS
		if iv.RX > 0 && iv.RTTMinUS > 0 && (r.RTTMinUS == 0 || iv.RTTMinUS < r.RTTMinUS) {
			r.RTTMinUS = iv.RTTMinUS
		}
		if iv.RTTMaxUS > r.RTTMaxUS {
			r.RTTMaxUS = iv.RTTMaxUS
		}
		if iv.Gap {
			r.Gaps++
		}
		if iv.Starved {
			r.Starved++
		}
		if d := iv.Duration(); d > r.MaxInterval {
			r.MaxInterval = d
		}
		if iv.Err != "" {
			if j, ok := errIdx[iv.Err]; ok {
				r.Errors[j].Count++
			} else {
				errIdx[iv.Err] = len(r.Errors)
				r.Errors = append(r.Errors, pingError{Msg: iv.Err, Count: 1, First: iv.End})
			}
		}
		if iv.Lost > 0 {
			r.LossyIntervals++
			if ep == nil {
				r.Episodes = append(r.Episodes, pingEpisode{Start: iv.Start})
				ep = &r.Episodes[len(r.Episodes)-1]
			}
			ep.End = iv.End
			ep.Intervals++
			ep.TX += iv.TX
			ep.Lost += iv.Lost
			ep.Late += iv.Late
			if iv.RTTMaxUS > ep.RTTMaxUS {
				ep.RTTMaxUS = iv.RTTMaxUS
			}
		} else {
			ep = nil
		}
	}
	if r.RX > 0 {
		r.RTTMeanUS = float64(rttSum) / float64(r.RX)
		variance := float64(rttSumSq)/float64(r.RX) - r.RTTMeanUS*r.RTTMeanUS
		if variance > 0 {
			r.RTTStddevUS = math.Sqrt(variance)
		}
	}
	return r
}

// runPingReport implements `msu -ping [-ping-intervals] [-from] [-to] FILE`.
func runPingReport(filename, fromFlag, toFlag string, allIntervals bool) error {
	pf, err := collectPingSamples(filename)
	if err != nil {
		return err
	}
	if len(pf.Targets) == 0 {
		return fmt.Errorf("%s: no ping monitor samples found (was msu-collect run with -ping?)", filename)
	}

	var from, to time.Time
	refDate := pf.Runs[0].Start
	if fromFlag != "" {
		if from, err = parseTimeFlag(fromFlag, refDate); err != nil {
			return err
		}
	}
	if toFlag != "" {
		if to, err = parseTimeFlag(toFlag, refDate); err != nil {
			return err
		}
	}

	var reports []*pingTargetReport
	for _, t := range pf.Targets {
		reports = append(reports, buildPingReport(t, pf.Samples[t], pf.Runs, from, to))
	}
	printPingReport(os.Stdout, filename, pf, reports, from, to, allIntervals)
	return nil
}

func fmtMS(us float64) string {
	return fmt.Sprintf("%.2fms", us/1000)
}

func fmtDur(d time.Duration) string {
	return d.Round(time.Second).String()
}

const pingTimeFmt = "2006-01-02 15:04:05"

// printPingReport writes the text report.
func printPingReport(w io.Writer, filename string, pf *pingFile, reports []*pingTargetReport, from, to time.Time, allIntervals bool) {
	fmt.Fprintln(w, "ICMP ping monitor report")
	fmt.Fprintln(w, "========================")
	fmt.Fprintf(w, "File:            %s\n", filename)
	if len(pf.Runs) > 0 {
		r0 := pf.Runs[0]
		fmt.Fprintf(w, "Collector:       msu-collect %s on %s (%s), sampling every %s, %d run(s)\n",
			r0.MsuVer, r0.Hostname, r0.Kernel, r0.Interval, len(pf.Runs))
		for i, run := range pf.Runs {
			fmt.Fprintf(w, "  run %d started %s UTC: %s\n", i+1, run.Start.Format(pingTimeFmt), strings.Join(run.CmdLine, " "))
		}
	}
	var first, last time.Time
	var intervalUS, timeoutUS uint64
	for _, r := range reports {
		if r.Samples == 0 {
			continue
		}
		if first.IsZero() || r.First.Before(first) {
			first = r.First
		}
		if r.Last.After(last) {
			last = r.Last
		}
		if intervalUS == 0 {
			intervalUS, timeoutUS = r.IntervalUS, r.TimeoutUS
		}
	}
	if intervalUS > 0 {
		fmt.Fprintf(w, "Ping settings:   one echo request every %s per target (%.1f/s), reply timeout %s\n",
			time.Duration(intervalUS)*time.Microsecond, 1e6/float64(intervalUS), time.Duration(timeoutUS)*time.Microsecond)
	}
	if !from.IsZero() || !to.IsZero() {
		fs, ts := "(start)", "(end)"
		if !from.IsZero() {
			fs = from.Format(pingTimeFmt)
		}
		if !to.IsZero() {
			ts = to.Format(pingTimeFmt)
		}
		fmt.Fprintf(w, "Time filter:     %s -> %s UTC\n", fs, ts)
	}
	if !first.IsZero() {
		fmt.Fprintf(w, "Monitored:       %s -> %s UTC (%s)\n", first.Format(pingTimeFmt), last.Format(pingTimeFmt), fmtDur(last.Sub(first)))
	}
	fmt.Fprintf(w, "Targets:         %d\n\n", len(reports))

	fmt.Fprintf(w, "%-16s %7s %8s %8s %7s %8s %6s %9s %9s %12s %5s\n",
		"TARGET", "SAMPLES", "TX", "RX", "LOST", "LOSS%", "LATE", "RTT_AVG", "RTT_MAX", "LOSSY_IVALS", "GAPS")
	fmt.Fprintln(w, strings.Repeat("-", 110))
	for _, r := range reports {
		fmt.Fprintf(w, "%-16s %7d %8d %8d %7d %7.3f%% %6d %9s %9s %12s %5d\n",
			r.Target, r.Samples, r.TX, r.RX, r.Lost, r.LossPct(), r.Late,
			fmtMS(r.RTTMeanUS), fmtMS(float64(r.RTTMaxUS)),
			fmt.Sprintf("%d/%d", r.LossyIntervals, len(r.Intervals)), r.Gaps)
	}
	fmt.Fprintln(w, strings.Repeat("-", 110))

	for _, r := range reports {
		fmt.Fprintf(w, "\nTarget %s", r.Target)
		if r.Mode != "" {
			fmt.Fprintf(w, "  (%s socket, icmp id %d)", r.Mode, r.ID)
		}
		fmt.Fprintln(w)
		if r.Samples == 0 {
			fmt.Fprintln(w, "  no samples in the selected time range")
			continue
		}
		fmt.Fprintf(w, "  period:   %s -> %s UTC (%s), %d samples\n",
			r.First.Format(pingTimeFmt), r.Last.Format(pingTimeFmt), fmtDur(r.Last.Sub(r.First)), r.Samples)
		fmt.Fprintf(w, "  counters: tx %d  rx %d  lost %d (%.3f%%)  late %d  dup %d  stray %d  tx_err %d  rx_err %d\n",
			r.TX, r.RX, r.Lost, r.LossPct(), r.Late, r.Dup, r.Stray, r.TXErr, r.RXErr)
		if r.RX > 0 {
			fmt.Fprintf(w, "  rtt:      mean %s  stddev %s  min %s  max %s\n",
				fmtMS(r.RTTMeanUS), fmtMS(r.RTTStddevUS), fmtMS(float64(r.RTTMinUS)), fmtMS(float64(r.RTTMaxUS)))
		} else {
			fmt.Fprintln(w, "  rtt:      no replies received")
		}
		gapNote := "none"
		if r.Gaps > 0 {
			gapNote = fmt.Sprintf("%d interval(s) longer than %.1fx the sampling interval", r.Gaps, pingGapFactor)
		}
		fmt.Fprintf(w, "  sampling: %d intervals, longest %s, gaps: %s", len(r.Intervals), fmtDur(r.MaxInterval), gapNote)
		if r.Starved > 0 {
			fmt.Fprintf(w, ", %d interval(s) with fewer than %.0f%% of the expected requests sent", r.Starved, pingStarvedFraction*100)
		}
		fmt.Fprintln(w)
		if len(r.Errors) == 0 {
			fmt.Fprintln(w, "  errors:   none")
		} else {
			fmt.Fprintln(w, "  errors:")
			for _, e := range r.Errors {
				fmt.Fprintf(w, "    %dx (first %s): %s\n", e.Count, e.First.Format(pingTimeFmt), e.Msg)
			}
		}

		if r.LossyIntervals == 0 {
			fmt.Fprintln(w, "  loss:     no interval with lost requests")
		} else {
			fmt.Fprintf(w, "  loss:     %d of %d intervals with lost requests, %d episode(s):\n",
				r.LossyIntervals, len(r.Intervals), len(r.Episodes))
			for _, e := range r.Episodes {
				fmt.Fprintf(w, "    %s -> %s (%s, %d interval(s)): lost %d of %d (%.2f%%), late %d, rtt max %s\n",
					e.Start.Format(pingTimeFmt), e.End.Format("15:04:05"), fmtDur(e.End.Sub(e.Start)), e.Intervals,
					e.Lost, e.TX, e.LossPct(), e.Late, fmtMS(float64(e.RTTMaxUS)))
			}
		}

		if allIntervals || r.LossyIntervals > 0 {
			title := "intervals with loss"
			if allIntervals {
				title = "all intervals"
			}
			fmt.Fprintf(w, "  %s:\n", title)
			fmt.Fprintf(w, "    %-19s %-8s %6s %5s %5s %5s %7s %9s %9s %9s  %s\n",
				"START", "END", "DUR", "TX", "RX", "LOST", "LOSS%", "RTT_AVG", "RTT_MIN", "RTT_MAX", "NOTE")
			for _, iv := range r.Intervals {
				if !allIntervals && iv.Lost == 0 {
					continue
				}
				var notes []string
				if iv.NewRun {
					notes = append(notes, "run start")
				}
				if iv.Gap {
					notes = append(notes, "gap")
				}
				if iv.Starved {
					notes = append(notes, "starved")
				}
				if iv.Late > 0 {
					notes = append(notes, fmt.Sprintf("late %d", iv.Late))
				}
				if iv.TXErr > 0 {
					notes = append(notes, fmt.Sprintf("tx_err %d", iv.TXErr))
				}
				if iv.RXErr > 0 {
					notes = append(notes, fmt.Sprintf("rx_err %d", iv.RXErr))
				}
				if iv.Dup > 0 {
					notes = append(notes, fmt.Sprintf("dup %d", iv.Dup))
				}
				if iv.Stray > 0 {
					notes = append(notes, fmt.Sprintf("stray %d", iv.Stray))
				}
				if iv.Err != "" {
					notes = append(notes, "err: "+iv.Err)
				}
				fmt.Fprintf(w, "    %-19s %-8s %6s %5d %5d %5d %6.2f%% %9s %9s %9s  %s\n",
					iv.Start.Format(pingTimeFmt), iv.End.Format("15:04:05"), fmtDur(iv.Duration()),
					iv.TX, iv.RX, iv.Lost, iv.LossPct(),
					fmtMS(iv.RTTAvgUS()), fmtMS(float64(iv.RTTMinUS)), fmtMS(float64(iv.RTTMaxUS)),
					strings.Join(notes, ", "))
			}
		}
	}
}

// pingChartSeries builds the per-target time series for the HTML report:
// loss % and mean/max RTT (ms) per sampling interval, stamped at the end
// of the interval. Keys are returned in display order.
func pingChartSeries(pf *pingFile) (map[string][]cpuDataPoint, []string) {
	series := make(map[string][]cpuDataPoint)
	var keys []string
	for _, t := range pf.Targets {
		lossKey := "ping " + t + " loss %"
		avgKey := "ping " + t + " rtt avg ms"
		maxKey := "ping " + t + " rtt max ms"
		keys = append(keys, lossKey, avgKey, maxKey)
		for _, iv := range pingIntervals(pf.Samples[t], pf.Runs) {
			series[lossKey] = append(series[lossKey], cpuDataPoint{Time: iv.End, Value: iv.LossPct()})
			series[avgKey] = append(series[avgKey], cpuDataPoint{Time: iv.End, Value: iv.RTTAvgUS() / 1000})
			series[maxKey] = append(series[maxKey], cpuDataPoint{Time: iv.End, Value: float64(iv.RTTMaxUS) / 1000})
		}
	}
	return series, keys
}
