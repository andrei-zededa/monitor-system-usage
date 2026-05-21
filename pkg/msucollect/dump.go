package msucollect

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/andrei-zededa/monitor-system-usage/pkg/msuformat"
)

// Dump reads an MSU CBOR file and writes a human-readable text representation
// of its samples to w.
func Dump(path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := msuformat.NewReader(f)

	printHeader := func(h *msuformat.Header) {
		hdrTS := time.Unix(0, h.TS).UTC().Format(time.RFC3339Nano)
		fmt.Fprintf(w, "MSU CBOR v%d  msu_ver=%s  ts=%s\n", h.V, h.MsuVer, hdrTS)
		fmt.Fprintf(w, "  hz=%d  psz=%d  cgroup_v%d\n", h.HZ, h.PSZ, h.CgroupV)
		fmt.Fprintf(w, "  host=%s  kernel=%s %s %s\n",
			h.Hostname, h.KernelOSType, h.KernelRelease, h.KernelVersion)
		fmt.Fprintf(w, "  interval=%s  flush_every=%d  cmdline=%v\n",
			time.Duration(h.IntervalNS), h.FlushEveryN, h.CmdLine)
		fmt.Fprintf(w, "  env_mode=%s  env_keys=%d\n\n", h.EnvMode, len(h.Env))
	}

	hdr, err := r.ReadHeader()
	if err != nil {
		return fmt.Errorf("reading header: %w", err)
	}
	printHeader(hdr)

	r.OnHeader = func(h *msuformat.Header) {
		fmt.Fprintln(w, "==================== new run ====================")
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
		fmt.Fprintf(w, "[%s] seq=%d sec=%s cmd=%q%s%s\n",
			ts, s.Seq, s.Section, s.Cmd, ns, errStr)

		if s.Out != "" {
			lines := strings.SplitAfter(s.Out, "\n")
			for _, line := range lines {
				if line != "" {
					fmt.Fprintf(w, "  %s", line)
					if !strings.HasSuffix(line, "\n") {
						fmt.Fprintln(w)
					}
				}
			}
		}
		fmt.Fprintln(w)
	}

	return nil
}
