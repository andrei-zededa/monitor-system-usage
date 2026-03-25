package msuformat

import "time"

const FormatVersion = 1

// Header is the first record written to an MSU CBOR file.
type Header struct {
	V        int    `cbor:"v"`
	Type     string `cbor:"type"`              // always "header"
	Ts       string `cbor:"ts"`                // RFC 3339 UTC
	Version  string `cbor:"msu_ver"`           // collector version
	HZ       int    `cbor:"hz"`                // CLK_TCK
	PSZ      int    `cbor:"psz"`               // PAGESIZE
	CgroupV  int    `cbor:"cgroup_v"`          // 1 or 2
	Hostname string `cbor:"hostname,omitempty"` // optional
}

// Sample is one collected command/file output.
type Sample struct {
	V       int    `cbor:"v"`
	Ts      string `cbor:"ts"`            // RFC 3339 UTC
	Seq     int64  `cbor:"seq"`           // monotonic interval counter
	Section string `cbor:"sec"`           // "A", "B", or "init"
	Cmd     string `cbor:"cmd"`           // e.g. "cat /proc/stat"
	NS      string `cbor:"ns,omitempty"`  // network namespace
	Out     string `cbor:"out"`           // raw output, newline-joined
	Err     string `cbor:"err,omitempty"` // error message if collection failed
}

// ParseTime parses the Ts field back to time.Time.
func (s *Sample) ParseTime() (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s.Ts)
}

// Now returns the current time formatted for the Ts field.
func Now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
