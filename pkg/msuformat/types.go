package msuformat

import "time"

// FormatVersion is the current MSU CBOR format version.
//
// v2 breaks compatibility with v1:
//   - Timestamps are int64 unix-nanoseconds instead of RFC 3339 strings.
//   - A "src" dictionary record is emitted between header and samples,
//     and samples reference a uint16 source ID instead of repeating
//     section/cmd/ns strings.
//   - The header carries additional invocation metadata
//     (interval, cmdline, env, kernel version).
const FormatVersion = 2

// Record type tags used in the "type" field of every record. These let the
// Reader dispatch between Header/SourceDef/Sample when decoding.
const (
	TypeHeader    = "header"
	TypeSourceDef = "src"
	TypeSample    = "s"
)

// EnvMode values for Header.EnvMode.
const (
	EnvModeFiltered = "filtered"
	EnvModeAll      = "all"
	EnvModeNone     = "none"
)

// Header is the first record written to an MSU CBOR file.
type Header struct {
	V    int    `cbor:"v"`    // V is the format version, always equal to FormatVersion.
	Type string `cbor:"type"` // Type will be equal to TypeHeader.
	TS   int64  `cbor:"ts"`   // TS is the the start time, in unix nanoseconds UTC.

	MsuVer  string `cbor:"msu_ver"`  // MsuVer is the msu-collect command (binary) version.
	HZ      int    `cbor:"hz"`       // CLK_TCK
	PSZ     int    `cbor:"psz"`      // PAGESIZE
	CgroupV int    `cbor:"cgroup_v"` // 1 or 2

	// Host identity (all read from /proc/sys/kernel/*).
	Hostname      string `cbor:"hostname,omitempty"`     // /proc/sys/kernel/hostname
	KernelOSType  string `cbor:"kern_ostype,omitempty"`  // /proc/sys/kernel/ostype
	KernelRelease string `cbor:"kern_release,omitempty"` // /proc/sys/kernel/osrelease
	KernelVersion string `cbor:"kern_version,omitempty"` // /proc/sys/kernel/version

	// Invocation context.
	IntervalNS  int64             `cbor:"interval_ns"`   // sampling interval in nanoseconds
	FlushEveryN int               `cbor:"flush_every_n"` // flush every N intervals
	CmdLine     []string          `cbor:"cmdline"`       // os.Args
	Env         map[string]string `cbor:"env,omitempty"` // environment, possibly filtered
	EnvMode     string            `cbor:"env_mode"`      // EnvModeFiltered | EnvModeAll | EnvModeNone
}

// SourceDef assigns a short numeric ID to a (Section, Cmd, NS) tuple. Emitted
// inline the first time the tuple is used; must appear before any Sample that
// references its ID.
type SourceDef struct {
	V       int    `cbor:"v"`    // V is the format version, always equal to FormatVersion.
	Type    string `cbor:"type"` // Type will be equal to TypeSourceDef.
	ID      uint16 `cbor:"id"`
	Section string `cbor:"sec"`          // "init", "A", or "B"
	Cmd     string `cbor:"cmd"`          // e.g. "cat /proc/stat"
	NS      string `cbor:"ns,omitempty"` // network namespace (empty for root)
}

// Sample is one collected command/file output, referencing a SourceDef
// by its ID.
type Sample struct {
	V     int    `cbor:"v"`    // V is the format version, always equal to FormatVersion.
	Type  string `cbor:"type"` // Type will be equal to TypeSample.
	TS    int64  `cbor:"ts"`   // TS is the sample timestamp in unix nanoseconds UTC.
	Seq   int64  `cbor:"seq"`  // Seq is a monotonic interval counter.
	SrcID uint16 `cbor:"src"`
	Out   string `cbor:"out"`           // raw output, newline-joined
	Err   string `cbor:"err,omitempty"` // error message if collection failed

	// Populated by Reader from the source dictionary — NEVER encoded.
	Section string `cbor:"-"`
	Cmd     string `cbor:"-"`
	NS      string `cbor:"-"`
}

// ParseTime returns the sample's timestamp as a time.Time.
func (s *Sample) ParseTime() time.Time {
	// NOTE: time.Unix(a, b) == time.Unix(0, 1_000_000_000 * a + b)
	//                       == time.Unix(x / 1_000_000_000, x % 1_000_000_000)
	//                       == time.Unix(0, x)
	return time.Unix(0, s.TS).UTC()
}

// NowNanos returns the current time as unix nanoseconds (UTC).
func NowNanos() int64 {
	return time.Now().UTC().UnixNano()
}
