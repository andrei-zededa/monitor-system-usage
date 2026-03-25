package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andrei-zededa/monitor-system-usage/pkg/msuformat"
)

var (
	version   = "dev" // version string, should be set at build time.
	commit    = ""    // commit id, should be set at build time.
	buildDate = ""
	builtBy   = ""
	treeState = ""
)

func main() {
	var (
		interval      = flag.Int("interval", 10, "Collection interval in seconds")
		flushInterval = flag.Int("flush-interval", 6, "Flush to disk every N collection intervals (default: 6 = every 60s at 10s interval)")
		namespaces    = flag.String("n", "", "Comma-separated list of network namespaces")
		outputFile    = flag.String("o", "", "Output file path (default: stdout)")
		showVer       = flag.Bool("version", false, "Show version and exit")
		dumpFile      = flag.String("dump", "", "Dump a CBOR file to human-readable text on stdout")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("msu-collect version %s\n", version)
		return
	}

	if *dumpFile != "" {
		if err := runDump(*dumpFile); err != nil {
			log.Fatal(err)
		}
		return
	}

	var nsList []string
	if *namespaces != "" {
		nsList = strings.Split(*namespaces, ",")
	}

	// Set up writer.
	var writer *msuformat.Writer
	if *outputFile != "" {
		var err error
		writer, err = msuformat.NewFileWriter(*outputFile)
		if err != nil {
			log.Fatalf("opening output file: %v", err)
		}
	} else {
		writer = msuformat.NewWriter(os.Stdout)
	}

	c := NewCollector(writer, time.Duration(*interval)*time.Second, *flushInterval, nsList)

	// Signal handling for clean shutdown.
	stop := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "received signal %v, shutting down...\n", sig)
		close(stop)
	}()

	fmt.Fprintf(os.Stderr, "msu-collect version=%s interval=%ds flush-interval=%d namespaces=%v\n",
		version, *interval, *flushInterval, nsList)

	if err := c.Run(stop); err != nil {
		log.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		log.Printf("warning: close failed: %v", err)
	}
}

// runDump reads a CBOR file and prints samples as human-readable text.
func runDump(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := msuformat.NewReader(f)

	hdr, err := r.ReadHeader()
	if err != nil {
		return fmt.Errorf("reading header: %w", err)
	}
	fmt.Printf("MSU CBOR v%d  msu_ver=%s  ts=%s  hz=%d  psz=%d  cgroup_v%d  host=%s\n\n",
		hdr.V, hdr.Version, hdr.Ts, hdr.HZ, hdr.PSZ, hdr.CgroupV, hdr.Hostname)

	for {
		s, err := r.Next()
		if err != nil {
			return fmt.Errorf("reading sample: %w", err)
		}
		if s == nil {
			break
		}

		ns := ""
		if s.NS != "" {
			ns = fmt.Sprintf(" <NS=%s>", s.NS)
		}
		errStr := ""
		if s.Err != "" {
			errStr = fmt.Sprintf(" [ERR: %s]", s.Err)
		}
		fmt.Printf("[%s] seq=%d sec=%s cmd=%q%s%s\n",
			s.Ts, s.Seq, s.Section, s.Cmd, ns, errStr)

		if s.Out != "" {
			lines := strings.SplitAfter(s.Out, "\n")
			for _, line := range lines {
				if line != "" {
					fmt.Printf("  %s", line)
					if !strings.HasSuffix(line, "\n") {
						fmt.Println()
					}
				}
			}
		}
		fmt.Println()
	}

	return nil
}
