package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
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

// envSecretRe matches environment variable names that commonly hold secrets.
// When -include-env=filtered (the default) these are dropped before the env
// map is recorded in the header (e.g. PASSWORD, CREDENTIAL, COOKIE).
var envSecretRe = regexp.MustCompile(`(?i)(TOKEN|KEY|SECRET|PASS|AUTH|CRED|COOK)`)

// collectEnv returns the environment to record in the header for the requested
// mode. Mode must be one of the msuformat.EnvMode* constants; an unknown value
// is treated as EnvModeFiltered.
func collectEnv(mode string) map[string]string {
	if mode == msuformat.EnvModeNone {
		return nil
	}
	raw := os.Environ()
	m := make(map[string]string, len(raw))
	for _, kv := range raw {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		k, v := kv[:i], kv[i+1:]
		if mode != msuformat.EnvModeAll && envSecretRe.MatchString(k) {
			continue
		}
		m[k] = v
	}
	return m
}

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
		return
	}

	if *dumpFile != "" {
		if err := runDump(*dumpFile); err != nil {
			log.Fatal(err)
		}
		return
	}

	switch *includeEnv {
	case msuformat.EnvModeFiltered, msuformat.EnvModeAll, msuformat.EnvModeNone:
	default:
		log.Fatalf("invalid -include-env value %q (allowed: filtered|all|none)", *includeEnv)
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

	cmdLine := append([]string(nil), os.Args...)
	env := collectEnv(*includeEnv)

	c := NewCollector(writer, time.Duration(*interval)*time.Second, *flushInterval,
		nsList, cmdLine, env, *includeEnv)

	// Signal handling for clean shutdown.
	stop := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "received signal %v, shutting down...\n", sig)
		close(stop)
	}()

	fmt.Fprintf(os.Stderr, "msu-collect version=%s interval=%ds flush-interval=%d namespaces=%v env=%s\n",
		version, *interval, *flushInterval, nsList, *includeEnv)

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

	printHeader := func(h *msuformat.Header) {
		hdrTS := time.Unix(0, h.TS).UTC().Format(time.RFC3339Nano)
		fmt.Printf("MSU CBOR v%d  msu_ver=%s  ts=%s\n", h.V, h.MsuVer, hdrTS)
		fmt.Printf("  hz=%d  psz=%d  cgroup_v%d\n", h.HZ, h.PSZ, h.CgroupV)
		fmt.Printf("  host=%s  kernel=%s %s %s\n",
			h.Hostname, h.KernelOSType, h.KernelRelease, h.KernelVersion)
		fmt.Printf("  interval=%s  flush_every=%d  cmdline=%v\n",
			time.Duration(h.IntervalNS), h.FlushEveryN, h.CmdLine)
		fmt.Printf("  env_mode=%s  env_keys=%d\n\n", h.EnvMode, len(h.Env))
	}

	hdr, err := r.ReadHeader()
	if err != nil {
		return fmt.Errorf("reading header: %w", err)
	}
	printHeader(hdr)

	r.OnHeader = func(h *msuformat.Header) {
		fmt.Println("==================== new run ====================")
		printHeader(h)
	}

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
		ts := time.Unix(0, s.TS).UTC().Format(time.RFC3339Nano)
		fmt.Printf("[%s] seq=%d sec=%s cmd=%q%s%s\n",
			ts, s.Seq, s.Section, s.Cmd, ns, errStr)

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
