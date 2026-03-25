package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/andrei-zededa/monitor-system-usage/pkg/msuformat"
)

// Collector manages the data collection loop.
type Collector struct {
	writer        *msuformat.Writer
	interval      time.Duration
	flushInterval int // flush every N collection intervals (0 = every interval)
	namespaces    []string
	cgroupV       int
	hz            int
	psz           int

	// Dynamic state refreshed each A section.
	interfaces   []string
	qemuPIDs     []int
	cgroupPaths  []string
	nsInterfaces map[string][]string // ns -> interfaces
}

// NewCollector creates a Collector.
func NewCollector(w *msuformat.Writer, interval time.Duration, flushInterval int, namespaces []string) *Collector {
	cgv := detectCgroupVersion()
	if flushInterval < 1 {
		flushInterval = 1
	}
	return &Collector{
		writer:        w,
		interval:      interval,
		flushInterval: flushInterval,
		namespaces:    namespaces,
		cgroupV:       cgv,
		hz:            getConf("CLK_TCK"),
		psz:           getConf("PAGESIZE"),
		nsInterfaces:  make(map[string][]string),
	}
}

// WriteHeader writes the initial header record.
func (c *Collector) WriteHeader() error {
	return c.writer.WriteHeader(&msuformat.Header{
		V:        msuformat.FormatVersion,
		Type:     "header",
		Ts:       msuformat.Now(),
		Version:  version,
		HZ:       c.hz,
		PSZ:      c.psz,
		CgroupV:  c.cgroupV,
		Hostname: getHostname(),
	})
}

// collectSource collects a single source and writes the sample.
func (c *Collector) collectSource(src Source, seq int64) {
	var out string
	var errMsg string

	switch src.Type {
	case "file":
		if src.NS != "" {
			o, err := readFileInNS(src.NS, src.Path)
			if err != nil {
				errMsg = err.Error()
			}
			out = o
		} else {
			o, err := readFile(src.Path)
			if err != nil {
				errMsg = err.Error()
			}
			out = o
		}
	case "exec":
		if src.NS != "" {
			o, err := runCmdInNS(src.NS, src.Args)
			if err != nil {
				errMsg = err.Error()
			}
			out = o
		} else {
			o, err := runCmd(src.Args)
			if err != nil {
				errMsg = err.Error()
			}
			out = o
		}
	}

	s := &msuformat.Sample{
		V:       msuformat.FormatVersion,
		Ts:      msuformat.Now(),
		Seq:     seq,
		Section: src.Section,
		Cmd:     src.Cmd,
		NS:      src.NS,
		Out:     out,
		Err:     errMsg,
	}

	if err := c.writer.WriteSample(s); err != nil {
		log.Printf("warning: failed to write sample for %q: %v", src.Cmd, err)
	}
}

// collectAll collects a list of sources sequentially.
func (c *Collector) collectAll(sources []Source, seq int64) {
	for _, src := range sources {
		c.collectSource(src, seq)
	}
}

// refreshDynamicState re-discovers interfaces, QEMU PIDs, and cgroup paths.
func (c *Collector) refreshDynamicState() {
	c.interfaces = discoverInterfaces()
	c.qemuPIDs = discoverQEMUPIDs()
	c.cgroupPaths = discoverCgroupPaths(c.cgroupV)

	for _, ns := range c.namespaces {
		c.nsInterfaces[ns] = discoverInterfacesIn(ns)
	}
}

// collectASection collects all A-section data.
func (c *Collector) collectASection(seq int64) {
	c.refreshDynamicState()

	// Refresh HZ/PSZ (the shell script does this each A section).
	c.hz = getConf("CLK_TCK")
	c.psz = getConf("PAGESIZE")

	// Static A sources.
	c.collectAll(staticASources(), seq)

	// Per-interface sources (root namespace).
	for _, intf := range c.interfaces {
		c.collectAll(interfaceSources(intf, ""), seq)
	}

	// Per-namespace sources.
	for _, ns := range c.namespaces {
		c.collectAll(nsSources(ns), seq)
		for _, intf := range c.nsInterfaces[ns] {
			c.collectAll(interfaceSources(intf, ns), seq)
		}
	}
}

// collectBSection collects all B-section data.
func (c *Collector) collectBSection(seq int64) {
	// Static B sources.
	c.collectAll(staticBSources(), seq)

	// Dynamic cgroup sources.
	c.collectAll(cgroupSources(c.cgroupPaths), seq)

	// Per-QEMU process sources.
	for _, pid := range c.qemuPIDs {
		c.collectAll(qemuSources(pid), seq)
	}
}

// Run executes the collection loop until stop is closed.
func (c *Collector) Run(stop <-chan struct{}) error {
	// Install tools (best effort, like the shell script).
	installTools()

	if err := c.WriteHeader(); err != nil {
		return fmt.Errorf("writing header: %w", err)
	}

	// One-time init collection.
	c.collectAll(initSources(), 0)
	if err := c.writer.Flush(); err != nil {
		return fmt.Errorf("flushing init data: %w", err)
	}

	var seq int64
	for {
		if seq%3 == 0 {
			c.collectASection(seq)
		}

		c.collectBSection(seq)

		if (seq+1)%int64(c.flushInterval) == 0 {
			if err := c.writer.Flush(); err != nil {
				log.Printf("warning: flush failed: %v", err)
			}
		}

		seq++

		select {
		case <-stop:
			return nil
		case <-time.After(c.interval):
		}
	}
}

// installTools runs apk add for required tools (best effort).
func installTools() {
	out, err := runCmd([]string{"apk", "add", "--no-cache", "musl-utils", "iproute2", "ethtool"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "apk add warning: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "%s\n", out)
	}
}
