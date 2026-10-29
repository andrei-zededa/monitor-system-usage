package msucollect

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func TestICMPChecksum(t *testing.T) {
	// Worked example from RFC 1071 section 3.
	got := icmpChecksum([]byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7})
	if got != 0x220d {
		t.Fatalf("checksum = %#04x, want 0x220d", got)
	}
	// Odd length pads the last byte on the right.
	if icmpChecksum([]byte{0x01}) != ^uint16(0x0100) {
		t.Fatalf("odd-length checksum wrong")
	}
}

func TestBuildAndParseEcho(t *testing.T) {
	now := time.Unix(1_700_000_000, 123)
	req := buildEchoRequest(0xbeef, 42, now)
	if len(req) != icmpHeaderLen+pingPayloadLen {
		t.Fatalf("len = %d, want %d", len(req), icmpHeaderLen+pingPayloadLen)
	}
	if req[0] != icmpEchoRequest || req[1] != 0 {
		t.Fatalf("type/code = %d/%d", req[0], req[1])
	}
	if icmpChecksum(req) != 0 {
		t.Fatalf("request checksum does not verify")
	}
	if got := binary.BigEndian.Uint64(req[8:]); got != uint64(now.UnixNano()) {
		t.Fatalf("payload timestamp = %d, want %d", got, now.UnixNano())
	}
	if _, _, ok := parseEchoReply(req); ok {
		t.Fatalf("echo request must not parse as a reply")
	}

	// Turn it into the reply the target would send back.
	rep := append([]byte(nil), req...)
	rep[0] = icmpEchoReply
	rep[2], rep[3] = 0, 0
	binary.BigEndian.PutUint16(rep[2:], icmpChecksum(rep))
	id, seq, ok := parseEchoReply(rep)
	if !ok || id != 0xbeef || seq != 42 {
		t.Fatalf("parse = (%d, %d, %v), want (0xbeef, 42, true)", id, seq, ok)
	}

	bad := append([]byte(nil), rep...)
	bad[20] ^= 0xff
	if _, _, ok := parseEchoReply(bad); ok {
		t.Fatalf("corrupted reply must not parse")
	}
	if _, _, ok := parseEchoReply(rep[:7]); ok {
		t.Fatalf("short message must not parse")
	}
}

func TestPingStateClassification(t *testing.T) {
	const timeout = 500 * time.Millisecond
	s := newPingState(timeout)
	t0 := time.Unix(1_700_000_000, 0)

	// In-time reply.
	s.sent(1, t0)
	s.reply(1, t0.Add(1*time.Millisecond))
	c := s.snapshot(t0.Add(2 * time.Millisecond))
	if c.TX != 1 || c.RX != 1 || c.Lost != 0 || c.Pending != 0 {
		t.Fatalf("after in-time reply: %+v", c)
	}
	if c.RTTSumUS != 1000 || c.RTTSumSqUS != 1_000_000 || c.RTTMin != time.Millisecond ||
		c.RTTMax != time.Millisecond || c.RTTLast != time.Millisecond {
		t.Fatalf("rtt accounting: %+v", c)
	}

	// Snapshot resets the per-interval min/max but not the cumulative sums.
	c = s.snapshot(t0.Add(3 * time.Millisecond))
	if c.RTTMin != 0 || c.RTTMax != 0 || c.RTTSumUS != 1000 || c.RTTLast != time.Millisecond {
		t.Fatalf("after reset: %+v", c)
	}

	// Timed out probe, later answered: Lost stays, Late increments.
	t1 := t0.Add(100 * time.Millisecond)
	s.sent(2, t1)
	if c := s.snapshot(t1.Add(timeout / 2)); c.Pending != 1 || c.Lost != 0 {
		t.Fatalf("probe should still be pending: %+v", c)
	}
	s.sweep(t1.Add(timeout + time.Millisecond))
	if s.c.Lost != 1 {
		t.Fatalf("lost = %d, want 1", s.c.Lost)
	}
	s.reply(2, t1.Add(timeout+2*time.Millisecond))
	if s.c.Lost != 1 || s.c.Late != 1 || s.c.RX != 1 {
		t.Fatalf("late reply: %+v", s.c)
	}
	// Second copy of the same reply is a duplicate.
	s.reply(2, t1.Add(timeout+3*time.Millisecond))
	if s.c.Dup != 1 {
		t.Fatalf("dup = %d, want 1", s.c.Dup)
	}
	// Duplicate of an in-time reply too.
	s.reply(1, t1)
	if s.c.Dup != 2 {
		t.Fatalf("dup = %d, want 2", s.c.Dup)
	}
	// Reply for a sequence we never sent.
	s.reply(99, t1)
	if s.c.Stray != 1 {
		t.Fatalf("stray = %d, want 1", s.c.Stray)
	}

	// Reply arriving past the deadline but before any sweep ran.
	t2 := t0.Add(200 * time.Millisecond)
	s.sent(3, t2)
	s.reply(3, t2.Add(timeout+time.Millisecond))
	if s.c.Lost != 2 || s.c.Late != 2 || s.c.RX != 1 {
		t.Fatalf("late-before-sweep: %+v", s.c)
	}

	// Sequence wrap while a probe is pending counts the old one as lost.
	t3 := t0.Add(300 * time.Millisecond)
	s.sent(4, t3)
	s.sent(4, t3.Add(time.Millisecond))
	if s.c.Lost != 3 || s.c.TX != 5 {
		t.Fatalf("wrap: %+v", s.c)
	}

	// Retention: after the window everything is forgotten and replies are stray.
	s.sweep(t3.Add(s.retention + time.Second))
	if len(s.probes) != 0 {
		t.Fatalf("probes retained: %d", len(s.probes))
	}
	s.reply(4, t3.Add(s.retention+time.Second))
	if s.c.Stray != 2 {
		t.Fatalf("stray after retention = %d, want 2", s.c.Stray)
	}
}

func TestNewPingMonitorRejectsNonIPv4(t *testing.T) {
	if _, err := NewPingMonitor(PingConfig{Target: net.ParseIP("::1")}); err == nil {
		t.Fatalf("IPv6 target accepted")
	}
	if _, err := NewPingMonitor(PingConfig{Target: nil}); err == nil {
		t.Fatalf("nil target accepted")
	}
	m, err := NewPingMonitor(PingConfig{Target: net.ParseIP("192.0.2.1")})
	if err != nil {
		t.Fatal(err)
	}
	if m.Cmd() != "ping 192.0.2.1" {
		t.Fatalf("Cmd = %q", m.Cmd())
	}
	if m.cfg.Interval != DefaultPingInterval || m.cfg.Timeout != DefaultPingTimeout {
		t.Fatalf("defaults not applied: %+v", m.cfg)
	}
}

func TestPingSampleFormat(t *testing.T) {
	m, err := NewPingMonitor(PingConfig{
		Target:   net.ParseIP("192.0.2.1"),
		Interval: 100 * time.Millisecond,
		Timeout:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Feed the state directly; the monitor was never started.
	t0 := time.Now()
	m.st.sent(1, t0)
	m.st.reply(1, t0.Add(1500*time.Microsecond))
	m.st.sent(2, t0)

	out, errStr := m.Sample()
	if errStr != "" {
		t.Fatalf("unexpected err %q", errStr)
	}
	if strings.HasSuffix(out, "\n") {
		t.Fatalf("output must not end with a newline")
	}
	want := map[string]string{
		"target":      "192.0.2.1",
		"mode":        "none",
		"interval_us": "100000",
		"timeout_us":  "500000",
		"tx":          "2",
		"rx":          "1",
		"lost":        "0",
		"pending":     "1",
		"rtt_sum_us":  "1500",
		"rtt_min_us":  "1500",
		"rtt_max_us":  "1500",
		"rtt_last_us": "1500",
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("line %q is not 'key value'", line)
		}
		got[f[0]] = f[1]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q\n%s", k, got[k], v, out)
		}
	}
}

// pingLoopback runs a monitor against 127.0.0.1 and returns once replies
// have been counted. It skips when the environment forbids both socket
// flavours (no CAP_NET_RAW and gid outside net.ipv4.ping_group_range).
func pingLoopback(t *testing.T, wantMode string) {
	t.Helper()
	m, err := NewPingMonitor(PingConfig{
		Target:   net.ParseIP("127.0.0.1"),
		Interval: 10 * time.Millisecond,
		Timeout:  200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	defer m.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		m.mu.Lock()
		openErr, sock := m.openErr, m.sock
		rx := m.st.c.RX
		m.mu.Unlock()
		if sock == nil && openErr != "" {
			if strings.Contains(openErr, "permitted") || strings.Contains(openErr, "denied") {
				t.Skipf("cannot open ICMP socket: %s", openErr)
			}
			t.Fatalf("cannot open ICMP socket: %s", openErr)
		}
		if sock != nil && sock.mode != wantMode {
			t.Skipf("socket opened in %s mode, test wants %s", sock.mode, wantMode)
		}
		if rx >= 10 {
			out, errStr := m.Sample()
			if errStr != "" {
				t.Fatalf("sample error: %s", errStr)
			}
			if !strings.Contains(out, "mode "+wantMode+"\n") {
				t.Fatalf("expected mode %s in:\n%s", wantMode, out)
			}
			c := m.Snapshot()
			if c.Lost != 0 || c.Stray != 0 || c.TXErr != 0 || c.RXErr != 0 {
				t.Fatalf("unexpected loss/errors on loopback: %+v", c)
			}
			if c.RTTSumUS == 0 || c.RTTLast == 0 {
				t.Fatalf("rtt not accounted: %+v", c)
			}
			t.Logf("mode=%s rx=%d tx=%d rtt_last=%s", wantMode, c.RX, c.TX, c.RTTLast)
			return
		}
	}
	t.Fatalf("no replies from loopback within 3s: %+v", m.Snapshot())
}

func TestPingMonitorLoopbackRaw(t *testing.T) {
	pingLoopback(t, "raw")
}

func TestPingMonitorLoopbackDgram(t *testing.T) {
	pingAllowRaw = false
	defer func() { pingAllowRaw = true }()
	pingLoopback(t, "dgram")
}
