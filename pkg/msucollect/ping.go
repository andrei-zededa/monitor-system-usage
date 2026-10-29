package msucollect

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Default ping monitor tuning: 10 echo requests per second, each considered
// lost when no reply arrives within half a second.
const (
	DefaultPingInterval = 100 * time.Millisecond
	DefaultPingTimeout  = 500 * time.Millisecond
)

const (
	icmpEchoRequest = 8
	icmpEchoReply   = 0
	icmpHeaderLen   = 8
	pingPayloadLen  = 56 // same as ping(8): a 64-byte ICMP message
	icmpFilterOpt   = 1  // ICMP_FILTER from <linux/icmp.h>, missing from package syscall

	// pingReopenDelay is how long a monitor waits before retrying to open
	// its socket after a failure (e.g. missing CAP_NET_RAW).
	pingReopenDelay = 5 * time.Second
	// pingMinRetention bounds how long a probe stays known after it was
	// sent so that late and duplicate replies can still be classified.
	pingMinRetention = 10 * time.Second
)

// pingAllowRaw lets tests force the unprivileged ICMP datagram socket path.
var pingAllowRaw = true

// pingIdentSeq makes every monitor in this process use a distinct ICMP echo
// identifier (only relevant in raw mode; the kernel picks one in dgram mode).
var pingIdentSeq uint32

// PingConfig configures one ICMP echo monitor.
type PingConfig struct {
	Target   net.IP        // IPv4 address to probe
	Interval time.Duration // time between echo requests (default DefaultPingInterval)
	Timeout  time.Duration // wait for a reply before counting the probe as lost (default DefaultPingTimeout)
}

// PingCounters is a snapshot of one monitor's counters. Every field is
// cumulative since the monitor started, except RTTMin/RTTMax which only
// cover replies received since the previous snapshot, and Pending which is
// the number of probes still waiting for a reply at snapshot time.
type PingCounters struct {
	TX    uint64 // echo requests handed to the kernel
	TXErr uint64 // send attempts that failed (e.g. network unreachable)
	RX    uint64 // echo replies received within Timeout
	RXErr uint64 // socket read errors
	Lost  uint64 // probes with no reply within Timeout
	Late  uint64 // replies that arrived after Timeout (already counted in Lost)
	Dup   uint64 // extra replies for an already answered probe
	Stray uint64 // replies carrying our identifier that match no known probe or come from another address

	Pending uint64 // probes still in flight (sent less than Timeout ago)

	RTTSumUS   uint64        // sum of in-time reply RTTs, microseconds
	RTTSumSqUS uint64        // sum of squared in-time reply RTTs, microseconds^2
	RTTLast    time.Duration // most recent in-time reply RTT
	RTTMin     time.Duration // smallest RTT since the previous snapshot; 0 when none
	RTTMax     time.Duration // largest RTT since the previous snapshot; 0 when none
}

type probeState uint8

const (
	probePending probeState = iota
	probeAnswered
	probeLost
)

type pingProbe struct {
	sent  time.Time
	state probeState
}

// pingState is the socket-free bookkeeping of a monitor: it classifies every
// sent probe and received reply and maintains the counters. It is not safe
// for concurrent use; PingMonitor guards it with a mutex.
type pingState struct {
	timeout   time.Duration
	retention time.Duration
	probes    map[uint16]*pingProbe
	c         PingCounters
	ivalRX    uint64 // in-time replies since the previous snapshot
}

func newPingState(timeout time.Duration) *pingState {
	retention := 4 * timeout
	if retention < pingMinRetention {
		retention = pingMinRetention
	}
	return &pingState{
		timeout:   timeout,
		retention: retention,
		probes:    make(map[uint16]*pingProbe),
	}
}

// sent records that the probe with the given sequence number was handed to
// the kernel at time now.
func (s *pingState) sent(seq uint16, now time.Time) {
	if old, ok := s.probes[seq]; ok && old.state == probePending {
		// The 16-bit sequence wrapped while this probe was still in
		// flight; its reply can never be matched again.
		s.c.Lost++
	}
	s.probes[seq] = &pingProbe{sent: now}
	s.c.TX++
}

// reply classifies an echo reply for the given sequence number received at
// time now.
func (s *pingState) reply(seq uint16, now time.Time) {
	p, ok := s.probes[seq]
	if !ok {
		s.c.Stray++
		return
	}
	switch p.state {
	case probePending:
		p.state = probeAnswered
		rtt := now.Sub(p.sent)
		if rtt < 0 {
			rtt = 0
		}
		if rtt > s.timeout {
			// Arrived after the deadline but before sweep() noticed.
			s.c.Lost++
			s.c.Late++
			return
		}
		s.c.RX++
		us := uint64(rtt / time.Microsecond)
		s.c.RTTSumUS += us
		s.c.RTTSumSqUS += us * us
		s.c.RTTLast = rtt
		if s.ivalRX == 0 || rtt < s.c.RTTMin {
			s.c.RTTMin = rtt
		}
		if rtt > s.c.RTTMax {
			s.c.RTTMax = rtt
		}
		s.ivalRX++
	case probeAnswered:
		s.c.Dup++
	case probeLost:
		p.state = probeAnswered
		s.c.Late++
	}
}

// sweep marks probes older than the timeout as lost and forgets probes
// older than the retention window.
func (s *pingState) sweep(now time.Time) {
	for seq, p := range s.probes {
		age := now.Sub(p.sent)
		if age > s.retention {
			delete(s.probes, seq)
			continue
		}
		if p.state == probePending && age > s.timeout {
			p.state = probeLost
			s.c.Lost++
		}
	}
}

// snapshot sweeps, returns a copy of the counters, and resets the
// per-interval RTT min/max.
func (s *pingState) snapshot(now time.Time) PingCounters {
	s.sweep(now)
	c := s.c
	for _, p := range s.probes {
		if p.state == probePending {
			c.Pending++
		}
	}
	s.c.RTTMin, s.c.RTTMax, s.ivalRX = 0, 0, 0
	return c
}

// icmpChecksum computes the RFC 1071 Internet checksum over b. For a
// message with a valid checksum field it returns 0.
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// buildEchoRequest returns a 64-byte ICMP echo request. The payload starts
// with the send time in unix nanoseconds (big-endian) and is zero-padded.
func buildEchoRequest(id, seq uint16, now time.Time) []byte {
	b := make([]byte, icmpHeaderLen+pingPayloadLen)
	b[0] = icmpEchoRequest
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	binary.BigEndian.PutUint64(b[8:], uint64(now.UnixNano()))
	binary.BigEndian.PutUint16(b[2:], icmpChecksum(b))
	return b
}

// parseEchoReply validates an ICMP message (without IP header) and returns
// its identifier and sequence number when it is a well-formed echo reply.
func parseEchoReply(b []byte) (id, seq uint16, ok bool) {
	if len(b) < icmpHeaderLen || b[0] != icmpEchoReply || b[1] != 0 {
		return 0, 0, false
	}
	if icmpChecksum(b) != 0 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint16(b[6:]), true
}

// pingSocket is an open ICMP socket plus the details that differ between
// the raw and datagram flavours.
type pingSocket struct {
	conn net.PacketConn
	dst  net.Addr
	mode string // "raw" (SOCK_RAW, needs CAP_NET_RAW) or "dgram" (unprivileged ping socket)
	id   uint16 // ICMP echo identifier carried by our requests and their replies
}

// openPingSocket opens a raw ICMP socket, falling back to an unprivileged
// IPPROTO_ICMP datagram socket (subject to net.ipv4.ping_group_range).
func openPingSocket(target net.IP, ident uint16) (*pingSocket, error) {
	var rawErr error
	if pingAllowRaw {
		conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
		if err == nil {
			setICMPFilter(conn)
			return &pingSocket{conn: conn, dst: &net.IPAddr{IP: target}, mode: "raw", id: ident}, nil
		}
		rawErr = err
	} else {
		rawErr = errors.New("disabled")
	}

	conn, err := openICMPDgram()
	if err != nil {
		return nil, fmt.Errorf("raw socket: %v; icmp dgram socket: %v", rawErr, err)
	}
	// The kernel assigned our identifier when the socket was bound.
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("icmp dgram socket: unexpected local address %v", conn.LocalAddr())
	}
	return &pingSocket{conn: conn, dst: &net.UDPAddr{IP: target}, mode: "dgram", id: uint16(la.Port)}, nil
}

// openICMPDgram creates an unprivileged "ping socket" and wraps it in a
// net.PacketConn (it shows up as a *net.UDPConn whose port is the ICMP id).
func openICMPDgram() (net.PacketConn, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_ICMP)
	if err != nil {
		return nil, os.NewSyscallError("socket", err)
	}
	// Binding to port 0 makes the kernel pick the echo identifier now, so
	// LocalAddr() reports it before the first send.
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{}); err != nil {
		syscall.Close(fd)
		return nil, os.NewSyscallError("bind", err)
	}
	f := os.NewFile(uintptr(fd), "icmp")
	defer f.Close() // FilePacketConn duplicates the descriptor
	return net.FilePacketConn(f)
}

// setICMPFilter asks the kernel to deliver only echo replies to a raw ICMP
// socket, which otherwise receives every ICMP message the host gets.
// Best effort: the receive loop filters by type anyway.
func setICMPFilter(conn net.PacketConn) {
	ipc, ok := conn.(*net.IPConn)
	if !ok {
		return
	}
	rc, err := ipc.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		// ICMP_FILTER is a bitmask of ICMP types to drop.
		mask := int(^uint32(1 << icmpEchoReply))
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_RAW, icmpFilterOpt, mask)
	})
}

// addrIP extracts the IP from the address types returned by ReadFrom.
func addrIP(a net.Addr) net.IP {
	switch a := a.(type) {
	case *net.IPAddr:
		return a.IP
	case *net.UDPAddr:
		return a.IP
	}
	return nil
}

// PingMonitor continuously sends ICMP echo requests to one IPv4 address
// from a background goroutine and keeps cumulative counters that the
// collector samples in every B section.
type PingMonitor struct {
	cfg   PingConfig
	ident uint16

	mu      sync.Mutex
	st      *pingState
	sock    *pingSocket // nil until the socket could be opened
	openErr string      // last socket open error; empty once opened
	ivalErr string      // last send/receive error since the previous sample

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewPingMonitor validates cfg and returns a monitor; call Start to begin
// probing.
func NewPingMonitor(cfg PingConfig) (*PingMonitor, error) {
	ip := cfg.Target.To4()
	if ip == nil {
		return nil, fmt.Errorf("ping target %q is not an IPv4 address", cfg.Target)
	}
	cfg.Target = ip
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultPingInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultPingTimeout
	}
	ident := uint16(os.Getpid()) + uint16(atomic.AddUint32(&pingIdentSeq, 1))
	return &PingMonitor{
		cfg:   cfg,
		ident: ident,
		st:    newPingState(cfg.Timeout),
		stop:  make(chan struct{}),
	}, nil
}

// Cmd returns the canonical source identifier used for this monitor's
// samples, e.g. "ping 10.0.0.1".
func (m *PingMonitor) Cmd() string {
	return "ping " + m.cfg.Target.String()
}

// Start launches the probing goroutines.
func (m *PingMonitor) Start() {
	m.wg.Add(1)
	go m.run()
}

// Stop ends probing and waits for the goroutines to exit.
func (m *PingMonitor) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
}

func (m *PingMonitor) run() {
	defer m.wg.Done()

	sock := m.open()
	if sock == nil {
		return // stopped before a socket could be opened
	}
	defer sock.conn.Close() // also unblocks the receiver

	m.wg.Add(1)
	go m.receive(sock)

	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	var seq uint16
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.send(sock, seq)
			seq++
		}
	}
}

// open retries opening the socket until it succeeds or the monitor is
// stopped, recording the last error for the samples.
func (m *PingMonitor) open() *pingSocket {
	for {
		sock, err := openPingSocket(m.cfg.Target, m.ident)
		if err == nil {
			m.mu.Lock()
			m.sock = sock
			m.openErr = ""
			m.mu.Unlock()
			log.Printf("ping %s: %s socket opened, id=%d interval=%s timeout=%s",
				m.cfg.Target, sock.mode, sock.id, m.cfg.Interval, m.cfg.Timeout)
			return sock
		}
		m.mu.Lock()
		if m.openErr != err.Error() {
			m.openErr = err.Error()
			log.Printf("warning: ping %s: %v (retrying every %s)", m.cfg.Target, err, pingReopenDelay)
		}
		m.mu.Unlock()
		select {
		case <-m.stop:
			return nil
		case <-time.After(pingReopenDelay):
		}
	}
}

// send transmits one echo request. The lock is held across the write so
// a reply cannot be processed before its probe is registered.
func (m *PingMonitor) send(sock *pingSocket, seq uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	pkt := buildEchoRequest(sock.id, seq, now)
	if _, err := sock.conn.WriteTo(pkt, sock.dst); err != nil {
		m.st.c.TXErr++
		m.ivalErr = err.Error()
	} else {
		m.st.sent(seq, now)
	}
	m.st.sweep(time.Now())
}

func (m *PingMonitor) receive(sock *pingSocket) {
	defer m.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, from, err := sock.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-m.stop:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			m.mu.Lock()
			m.st.c.RXErr++
			m.ivalErr = err.Error()
			m.mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		id, seq, ok := parseEchoReply(buf[:n])
		if !ok || id != sock.id {
			continue // not an echo reply, or someone else's
		}
		now := time.Now()
		m.mu.Lock()
		if addrIP(from).Equal(m.cfg.Target) {
			m.st.reply(seq, now)
		} else {
			m.st.c.Stray++
		}
		m.mu.Unlock()
	}
}

// Snapshot returns the current counters and resets the per-interval RTT
// min/max.
func (m *PingMonitor) Snapshot() PingCounters {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st.snapshot(time.Now())
}

// Sample returns the counters formatted as "key value" lines (one per
// line, like /proc/vmstat) plus an error string: the socket open error
// while the socket is not available, otherwise the last send/receive
// error since the previous sample.
func (m *PingMonitor) Sample() (out, errStr string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.st.snapshot(time.Now())
	mode, id := "none", uint16(0)
	if m.sock != nil {
		mode, id = m.sock.mode, m.sock.id
		errStr = m.ivalErr
	} else {
		errStr = m.openErr
	}
	m.ivalErr = ""

	var b strings.Builder
	fmt.Fprintf(&b, "target %s\n", m.cfg.Target)
	fmt.Fprintf(&b, "mode %s\n", mode)
	fmt.Fprintf(&b, "id %d\n", id)
	fmt.Fprintf(&b, "interval_us %d\n", m.cfg.Interval/time.Microsecond)
	fmt.Fprintf(&b, "timeout_us %d\n", m.cfg.Timeout/time.Microsecond)
	fmt.Fprintf(&b, "tx %d\n", c.TX)
	fmt.Fprintf(&b, "tx_err %d\n", c.TXErr)
	fmt.Fprintf(&b, "rx %d\n", c.RX)
	fmt.Fprintf(&b, "rx_err %d\n", c.RXErr)
	fmt.Fprintf(&b, "lost %d\n", c.Lost)
	fmt.Fprintf(&b, "late %d\n", c.Late)
	fmt.Fprintf(&b, "dup %d\n", c.Dup)
	fmt.Fprintf(&b, "stray %d\n", c.Stray)
	fmt.Fprintf(&b, "pending %d\n", c.Pending)
	fmt.Fprintf(&b, "rtt_sum_us %d\n", c.RTTSumUS)
	fmt.Fprintf(&b, "rtt_sumsq_us %d\n", c.RTTSumSqUS)
	fmt.Fprintf(&b, "rtt_last_us %d\n", c.RTTLast/time.Microsecond)
	fmt.Fprintf(&b, "rtt_min_us %d\n", c.RTTMin/time.Microsecond)
	fmt.Fprintf(&b, "rtt_max_us %d", c.RTTMax/time.Microsecond)
	return b.String(), errStr
}
