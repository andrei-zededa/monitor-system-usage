package msuformat

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	start := time.Now().UTC().UnixNano()

	if err := w.WriteHeader(&Header{
		TS:            start,
		MsuVer:        "test-1.0",
		HZ:            100,
		PSZ:           4096,
		CgroupV:       2,
		Hostname:      "host-under-test",
		KernelOSType:  "Linux",
		KernelRelease: "6.18.19",
		KernelVersion: "#1 SMP test",
		IntervalNS:    int64(10 * time.Second),
		FlushEveryN:   6,
		CmdLine:       []string{"msu-collect", "-interval", "10"},
		Env:           map[string]string{"PATH": "/usr/bin", "HOME": "/root"},
		EnvMode:       EnvModeFiltered,
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Three distinct tuples; reuse the first one twice to verify the
	// dictionary dedupes it to a single SourceDef.
	tuples := []struct {
		section, cmd, ns string
		out              string
	}{
		{"A", "cat /proc/stat", "", "cpu 1 2 3\n"},
		{"B", "cat /proc/meminfo", "", "MemTotal: 1 kB\n"},
		{"A", "cat /proc/stat", "", "cpu 4 5 6\n"},
		{"A", "cat /proc/net/dev", "ns1", "eth0: 1 2 3\n"},
		{"A", "cat /proc/stat", "", "cpu 7 8 9\n"},
	}
	for i, tp := range tuples {
		ts := start + int64(i+1)*1_000_000
		if err := w.WriteSample(tp.section, tp.cmd, tp.ns, int64(i), ts, tp.out, ""); err != nil {
			t.Fatalf("WriteSample %d: %v", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	r := NewReader(&buf)
	hdr, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	if hdr.V != FormatVersion {
		t.Errorf("header V = %d, want %d", hdr.V, FormatVersion)
	}
	if hdr.TS != start {
		t.Errorf("header Ts = %d, want %d", hdr.TS, start)
	}
	if hdr.MsuVer != "test-1.0" {
		t.Errorf("header MsuVer = %q", hdr.MsuVer)
	}
	if hdr.KernelRelease != "6.18.19" {
		t.Errorf("header KernelRelease = %q", hdr.KernelRelease)
	}
	if hdr.IntervalNS != int64(10*time.Second) {
		t.Errorf("header IntervalNS = %d", hdr.IntervalNS)
	}
	if hdr.EnvMode != EnvModeFiltered {
		t.Errorf("header EnvMode = %q", hdr.EnvMode)
	}
	if len(hdr.CmdLine) != 3 || hdr.CmdLine[0] != "msu-collect" {
		t.Errorf("header CmdLine = %v", hdr.CmdLine)
	}
	if hdr.Env["PATH"] != "/usr/bin" {
		t.Errorf("header Env[PATH] = %q", hdr.Env["PATH"])
	}

	var got []Sample
	for {
		s, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if s == nil {
			break
		}
		got = append(got, *s)
	}

	if len(got) != len(tuples) {
		t.Fatalf("got %d samples, want %d", len(got), len(tuples))
	}
	for i, tp := range tuples {
		if got[i].Section != tp.section {
			t.Errorf("sample %d Section = %q, want %q", i, got[i].Section, tp.section)
		}
		if got[i].Cmd != tp.cmd {
			t.Errorf("sample %d Cmd = %q, want %q", i, got[i].Cmd, tp.cmd)
		}
		if got[i].NS != tp.ns {
			t.Errorf("sample %d NS = %q, want %q", i, got[i].NS, tp.ns)
		}
		if got[i].Out != tp.out {
			t.Errorf("sample %d Out = %q, want %q", i, got[i].Out, tp.out)
		}
		if got[i].Seq != int64(i) {
			t.Errorf("sample %d Seq = %d, want %d", i, got[i].Seq, i)
		}
		wantTS := start + int64(i+1)*1_000_000
		if got[i].TS != wantTS {
			t.Errorf("sample %d TS = %d, want %d", i, got[i].TS, wantTS)
		}

		gotTime := got[i].ParseTime()
		if gotTime.UnixNano() != wantTS {
			t.Errorf("sample %d ParseTime %v != wantTS %d", i, gotTime, wantTS)
		}
	}
}

func TestSourceDictionaryDedupes(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	if err := w.WriteHeader(&Header{TS: 0, EnvMode: EnvModeNone}); err != nil {
		t.Fatal(err)
	}
	// Same tuple 10 times — should produce 1 SourceDef + 10 Samples.
	for i := 0; i < 10; i++ {
		if err := w.WriteSample("A", "cat /x", "", int64(i), int64(i), "out", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	dec := cbor.NewDecoder(&buf)
	counts := map[string]int{}
	for {
		var raw cbor.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		var env envelope
		if err := cbor.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		counts[env.Type]++
	}
	if counts[TypeHeader] != 1 {
		t.Errorf("header count = %d, want 1", counts[TypeHeader])
	}
	if counts[TypeSourceDef] != 1 {
		t.Errorf("source def count = %d, want 1 (should dedupe)", counts[TypeSourceDef])
	}
	if counts[TypeSample] != 10 {
		t.Errorf("sample count = %d, want 10", counts[TypeSample])
	}
}

func TestUnsupportedVersion(t *testing.T) {
	// Simulate a v1 file by writing a header-shaped record with V=1.
	em, _ := cbor.CoreDetEncOptions().EncMode()
	type v1Header struct {
		V    int    `cbor:"v"`
		Type string `cbor:"type"`
		Ts   string `cbor:"ts"`
	}
	data, err := em.Marshal(&v1Header{V: 1, Type: "header", Ts: "2026-04-20T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}

	r := NewReader(bytes.NewReader(data))
	_, err = r.ReadHeader()
	if err == nil {
		t.Fatal("expected error for v1 file, got nil")
	}
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("error %v does not wrap ErrUnsupportedVersion", err)
	}
	if !strings.Contains(err.Error(), "v1") || !strings.Contains(err.Error(), "v2") {
		t.Errorf("error message missing version numbers: %v", err)
	}
}

func TestTruncatedTailHandled(t *testing.T) {
	// Build a valid stream, then chop off the last few bytes to simulate
	// a crash during WriteSample. Reader should return (nil, nil) rather
	// than an error when the trailing record is partial.
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WriteHeader(&Header{TS: 0, EnvMode: EnvModeNone}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := w.WriteSample("A", "cat /x", "", int64(i), int64(i), "data", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	full := buf.Bytes()
	truncated := full[:len(full)-4] // chop last record mid-way

	r := NewReader(bytes.NewReader(truncated))
	if _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader after truncation: %v", err)
	}
	count := 0
	for {
		s, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if s == nil {
			break
		}
		count++
	}
	// Expect at most 3 samples; the last one may be lost due to truncation.
	if count < 2 || count > 3 {
		t.Errorf("got %d samples from truncated stream, want 2 or 3", count)
	}
}
