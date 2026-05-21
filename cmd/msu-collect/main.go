package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andrei-zededa/monitor-system-usage/pkg/msucollect"
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
		includeEnv    = flag.String("include-env", msuformat.EnvModeFiltered,
			"Environment variables to record in header: filtered|all|none")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("msu-collect version %s\n", version)
		_ = commit
		_ = buildDate
		_ = builtBy
		_ = treeState
		return
	}

	if *dumpFile != "" {
		if err := msucollect.Dump(*dumpFile, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	var nsList []string
	if *namespaces != "" {
		nsList = strings.Split(*namespaces, ",")
	}

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

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "received signal %v, shutting down...\n", sig)
		cancel()
	}()

	fmt.Fprintf(os.Stderr, "msu-collect version=%s interval=%ds flush-interval=%d namespaces=%v env=%s\n",
		version, *interval, *flushInterval, nsList, *includeEnv)

	cfg := msucollect.Config{
		Writer:      writer,
		Interval:    time.Duration(*interval) * time.Second,
		FlushEveryN: *flushInterval,
		Namespaces:  nsList,
		IncludeEnv:  *includeEnv,
		Version:     version,
	}
	if err := msucollect.Run(ctx, cfg); err != nil {
		log.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		log.Printf("warning: close failed: %v", err)
	}
}
