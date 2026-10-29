package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func pingLines(c pingCounters) []string {
	return strings.Split(strings.Join([]string{
		"target " + c.Target,
		"mode " + c.Mode,
		"id 5662",
		"interval_us 100000",
		"timeout_us 300000",
		"tx " + u(c.TX),
		"tx_err " + u(c.TXErr),
		"rx " + u(c.RX),
		"rx_err " + u(c.RXErr),
		"lost " + u(c.Lost),
		"late " + u(c.Late),
		"dup " + u(c.Dup),
		"stray " + u(c.Stray),
		"pending " + u(c.Pending),
		"rtt_sum_us " + u(c.RTTSumUS),
		"rtt_sumsq_us " + u(c.RTTSumSqUS),
		"rtt_last_us " + u(c.RTTLastUS),
		"rtt_min_us " + u(c.RTTMinUS),
		"rtt_max_us " + u(c.RTTMaxUS),
	}, "\n"), "\n")
}

func u(n uint64) string { return strings.TrimSpace(strings.Repeat(" ", 0) + itoa(n)) }

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestParsePingCounters(t *testing.T) {
	in := pingCounters{Target: "10.0.0.1", Mode: "raw", TX: 61, RX: 60, Lost: 1, Late: 1,
		RTTSumUS: 12345, RTTSumSqUS: 9999, RTTLastUS: 200, RTTMinUS: 150, RTTMaxUS: 450, Pending: 2}
	c, ok := parsePingCounters(pingLines(in))
	if !ok {
		t.Fatalf("parse failed")
	}
	if c.Target != "10.0.0.1" || c.Mode != "raw" || c.ID != 5662 || c.IntervalUS != 100000 || c.TimeoutUS != 300000 {
		t.Fatalf("header fields wrong: %+v", c)
	}
	if c.TX != 61 || c.RX != 60 || c.Lost != 1 || c.Late != 1 || c.Pending != 2 ||
		c.RTTSumUS != 12345 || c.RTTSumSqUS != 9999 || c.RTTLastUS != 200 || c.RTTMinUS != 150 || c.RTTMaxUS != 450 {
		t.Fatalf("counters wrong: %+v", c)
	}
	if _, ok := parsePingCounters([]string{"Architecture: x86_64", "CPU(s): 4"}); ok {
		t.Fatalf("non-ping output must not parse")
	}
}

func TestSortPingTargets(t *testing.T) {
	ts := []string{"172.19.2.65", "172.19.2.1", "10.0.0.9", "172.19.2.62", "host.example"}
	sortPingTargets(ts)
	want := "10.0.0.9 172.19.2.1 172.19.2.62 172.19.2.65 host.example"
	if got := strings.Join(ts, " "); got != want {
		t.Fatalf("sorted = %q, want %q", got, want)
	}
}

// synthSamples builds samples 10s apart starting at t0 from cumulative
// counters, with the run start 15s before the first sample.
func synthSamples(t0 time.Time, cs []pingCounters) ([]pingSample, []pingRun) {
	runs := []pingRun{{Start: t0.Add(-15 * time.Second), Interval: 10 * time.Second, MsuVer: "test"}}
	var ss []pingSample
	for i, c := range cs {
		c.Mode = "raw"
		c.IntervalUS = 100000
		c.TimeoutUS = 300000
		ss = append(ss, pingSample{Time: t0.Add(time.Duration(i) * 10 * time.Second), Seq: int64(i), C: c})
	}
	return ss, runs
}

func TestPingIntervalsAndReport(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	// Cumulative counters: 150 requests during init+first interval, then 100
	// per 10s. Interval 2 loses 3 (1 late), interval 3 loses 2, interval 4 is
	// clean, interval 5 loses 1 again (new episode).
	cs := []pingCounters{
		{TX: 150, RX: 150, RTTSumUS: 150 * 400, RTTSumSqUS: 150 * 400 * 400, RTTMinUS: 300, RTTMaxUS: 500},
		{TX: 250, RX: 250, RTTSumUS: 250 * 400, RTTSumSqUS: 250 * 400 * 400, RTTMinUS: 310, RTTMaxUS: 480},
		{TX: 350, RX: 347, Lost: 3, Late: 1, RTTSumUS: 347 * 400, RTTSumSqUS: 347 * 400 * 400, RTTMinUS: 320, RTTMaxUS: 2500},
		{TX: 450, RX: 445, Lost: 5, Late: 1, RTTSumUS: 445 * 400, RTTSumSqUS: 445 * 400 * 400, RTTMinUS: 330, RTTMaxUS: 1200},
		{TX: 550, RX: 545, Lost: 5, Late: 1, RTTSumUS: 545 * 400, RTTSumSqUS: 545 * 400 * 400, RTTMinUS: 300, RTTMaxUS: 450},
		{TX: 650, RX: 644, Lost: 6, Late: 1, RTTSumUS: 644 * 400, RTTSumSqUS: 644 * 400 * 400, RTTMinUS: 300, RTTMaxUS: 700},
	}
	ss, runs := synthSamples(t0, cs)
	ivs := pingIntervals(ss, runs)
	if len(ivs) != len(cs) {
		t.Fatalf("intervals = %d, want %d", len(ivs), len(cs))
	}
	if !ivs[0].NewRun || !ivs[0].Start.Equal(runs[0].Start) || ivs[0].TX != 150 {
		t.Fatalf("first interval must start at run start with full counters: %+v", ivs[0])
	}
	if ivs[1].NewRun || ivs[1].TX != 100 || ivs[1].Lost != 0 || ivs[1].Duration() != 10*time.Second {
		t.Fatalf("second interval wrong: %+v", ivs[1])
	}
	if ivs[2].Lost != 3 || ivs[2].Late != 1 || ivs[2].RX != 97 || ivs[2].RTTMaxUS != 2500 {
		t.Fatalf("lossy interval wrong: %+v", ivs[2])
	}
	if got := ivs[2].LossPct(); got < 2.99 || got > 3.01 {
		t.Fatalf("loss pct = %v, want 3", got)
	}
	if got := ivs[2].RTTAvgUS(); got != 400 {
		t.Fatalf("rtt avg = %v, want 400", got)
	}
	for _, iv := range ivs {
		if iv.Gap || iv.Starved {
			t.Fatalf("unexpected gap/starved flag: %+v", iv)
		}
	}

	r := buildPingReport("10.0.0.1", ss, runs, time.Time{}, time.Time{})
	if r.TX != 650 || r.RX != 644 || r.Lost != 6 || r.Late != 1 {
		t.Fatalf("totals wrong: %+v", r)
	}
	if r.LossyIntervals != 3 || len(r.Episodes) != 2 {
		t.Fatalf("lossy intervals = %d, episodes = %d, want 3 and 2", r.LossyIntervals, len(r.Episodes))
	}
	if e := r.Episodes[0]; e.Intervals != 2 || e.Lost != 5 || e.TX != 200 || e.RTTMaxUS != 2500 ||
		!e.Start.Equal(t0.Add(10*time.Second)) || !e.End.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("episode 0 wrong: %+v", e)
	}
	if r.RTTMeanUS != 400 || r.RTTStddevUS != 0 || r.RTTMinUS != 300 || r.RTTMaxUS != 2500 {
		t.Fatalf("rtt stats wrong: mean=%v stddev=%v min=%d max=%d", r.RTTMeanUS, r.RTTStddevUS, r.RTTMinUS, r.RTTMaxUS)
	}
	if r.Mode != "raw" || r.IntervalUS != 100000 || r.TimeoutUS != 300000 {
		t.Fatalf("socket details wrong: %+v", r)
	}

	// Time filter keeps only intervals ending in the window.
	rf := buildPingReport("10.0.0.1", ss, runs, t0.Add(20*time.Second), t0.Add(30*time.Second))
	if rf.Samples != 2 || rf.TX != 200 || rf.Lost != 5 {
		t.Fatalf("filtered report wrong: samples=%d tx=%d lost=%d", rf.Samples, rf.TX, rf.Lost)
	}

	var b bytes.Buffer
	pf := &pingFile{Runs: runs, Targets: []string{"10.0.0.1"}, Samples: map[string][]pingSample{"10.0.0.1": ss}}
	printPingReport(&b, "test.cbor", pf, []*pingTargetReport{r}, time.Time{}, time.Time{}, false)
	out := b.String()
	for _, want := range []string{"Target 10.0.0.1", "lost 6 (0.923%)", "2 episode(s)", "late 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report output missing %q:\n%s", want, out)
		}
	}
}

func TestPingIntervalsRestartGapStarved(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	runs := []pingRun{
		{Start: t0.Add(-5 * time.Second), Interval: 10 * time.Second},
		{Start: t0.Add(100 * time.Second), Interval: 10 * time.Second},
	}
	mk := func(ts time.Time, run int, tx, rx, lost uint64) pingSample {
		return pingSample{Time: ts, Run: run, C: pingCounters{Mode: "raw", IntervalUS: 100000, TX: tx, RX: rx, Lost: lost}}
	}
	ss := []pingSample{
		mk(t0, 0, 50, 50, 0),
		mk(t0.Add(10*time.Second), 0, 150, 150, 0),
		// Collector stalled for 50s and only 200 requests went out instead of ~500.
		mk(t0.Add(60*time.Second), 0, 350, 350, 0),
		// Second run (new header): counters restart.
		mk(t0.Add(110*time.Second), 1, 100, 100, 0),
		mk(t0.Add(120*time.Second), 1, 200, 199, 1),
	}
	ivs := pingIntervals(ss, runs)
	if len(ivs) != 5 {
		t.Fatalf("intervals = %d", len(ivs))
	}
	if !ivs[2].Gap || !ivs[2].Starved || ivs[2].TX != 200 || ivs[2].Duration() != 50*time.Second {
		t.Fatalf("stalled interval not flagged: %+v", ivs[2])
	}
	if !ivs[3].NewRun || ivs[3].TX != 100 || !ivs[3].Start.Equal(runs[1].Start) || ivs[3].Gap {
		t.Fatalf("new run interval wrong: %+v", ivs[3])
	}
	if ivs[4].NewRun || ivs[4].TX != 100 || ivs[4].Lost != 1 {
		t.Fatalf("interval after restart wrong: %+v", ivs[4])
	}

	// A counter reset without a new header (same run index) is detected too.
	ss2 := []pingSample{mk(t0, 0, 500, 500, 2), mk(t0.Add(10*time.Second), 0, 100, 100, 0)}
	ivs2 := pingIntervals(ss2, runs)
	if !ivs2[1].NewRun || ivs2[1].TX != 100 || ivs2[1].Lost != 0 {
		t.Fatalf("counter restart not detected: %+v", ivs2[1])
	}

	r := buildPingReport("10.0.0.1", ss, runs, time.Time{}, time.Time{})
	if r.Gaps != 1 || r.Starved != 1 || r.TX != 550 || r.Lost != 1 || r.MaxInterval != 50*time.Second {
		t.Fatalf("report wrong: gaps=%d starved=%d tx=%d lost=%d max=%s", r.Gaps, r.Starved, r.TX, r.Lost, r.MaxInterval)
	}
}
