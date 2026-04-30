package msuformat

import (
	"bufio"
	"io"
	"os"

	"github.com/fxamacker/cbor/v2"
)

// sourceKey uniquely identifies a (Section, Cmd, NS) tuple for dictionary
// lookups. NS is part of the key so the same command in different network
// namespaces gets distinct IDs.
type sourceKey struct {
	Section, Cmd, NS string
}

// Writer writes CBOR-encoded records to a buffered output. It owns the
// source dictionary: the first time a given (section, cmd, ns) tuple is
// used with WriteSample, a SourceDef record is emitted inline just
// before the Sample that references it. This keeps the stream valid at
// every record boundary — a truncated tail never leaves a dangling ID.
type Writer struct {
	bw  *bufio.Writer
	f   *os.File // non-nil only when created via NewFileWriter
	enc cbor.EncMode

	srcs   map[sourceKey]uint16
	nextID uint16
}

func newWriter(w io.Writer) *Writer {
	em, _ := cbor.CoreDetEncOptions().EncMode()
	return &Writer{
		bw:   bufio.NewWriterSize(w, 64*1024),
		enc:  em,
		srcs: make(map[sourceKey]uint16),
	}
}

// NewWriter creates a Writer that writes to w (e.g. os.Stdout).
func NewWriter(w io.Writer) *Writer {
	return newWriter(w)
}

// NewFileWriter creates a Writer backed by the given file path (append mode).
func NewFileWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	wr := newWriter(f)
	wr.f = f
	return wr, nil
}

// WriteHeader writes the Header record. Must be called exactly once,
// before any WriteSample call.
func (w *Writer) WriteHeader(h *Header) error {
	h.V = FormatVersion
	h.Type = TypeHeader
	data, err := w.enc.Marshal(h)
	if err != nil {
		return err
	}
	_, err = w.bw.Write(data)
	return err
}

// WriteSample writes one sample. On first use of a given (section, cmd,
// ns) tuple, a SourceDef record is emitted before the Sample.
func (w *Writer) WriteSample(section, cmd, ns string, seq, tsNanos int64, out, errStr string) error {
	key := sourceKey{Section: section, Cmd: cmd, NS: ns}
	id, ok := w.srcs[key]
	if !ok {
		id = w.nextID
		w.nextID++
		w.srcs[key] = id

		def := &SourceDef{
			V:       FormatVersion,
			Type:    TypeSourceDef,
			ID:      id,
			Section: section,
			Cmd:     cmd,
			NS:      ns,
		}
		data, err := w.enc.Marshal(def)
		if err != nil {
			return err
		}
		if _, err := w.bw.Write(data); err != nil {
			return err
		}
	}

	s := &Sample{
		V:     FormatVersion,
		Type:  TypeSample,
		TS:    tsNanos,
		Seq:   seq,
		SrcID: id,
		Out:   out,
		Err:   errStr,
	}
	data, err := w.enc.Marshal(s)
	if err != nil {
		return err
	}
	_, err = w.bw.Write(data)
	return err
}

// Flush flushes the buffer and fsyncs the underlying file (if file-backed).
func (w *Writer) Flush() error {
	if err := w.bw.Flush(); err != nil {
		return err
	}
	if w.f != nil {
		return w.f.Sync()
	}
	return nil
}

// Close flushes and closes the underlying file (if file-backed).
func (w *Writer) Close() error {
	if err := w.Flush(); err != nil {
		return err
	}
	if w.f != nil {
		return w.f.Close()
	}
	return nil
}
