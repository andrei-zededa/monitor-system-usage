package msuformat

import (
	"bufio"
	"io"
	"os"

	"github.com/fxamacker/cbor/v2"
)

// Writer writes CBOR-encoded records to a buffered output.
type Writer struct {
	bw   *bufio.Writer
	f    *os.File // non-nil only when created via NewFileWriter
	enc  cbor.EncMode
}

func newWriter(w io.Writer) *Writer {
	em, _ := cbor.CoreDetEncOptions().EncMode()
	return &Writer{
		bw:  bufio.NewWriterSize(w, 64*1024),
		enc: em,
	}
}

// NewWriter creates a Writer that writes to w (e.g. os.Stdout).
func NewWriter(w io.Writer) *Writer {
	return newWriter(w)
}

// NewFileWriter creates a Writer backed by the given file path (append mode).
func NewFileWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	wr := newWriter(f)
	wr.f = f
	return wr, nil
}

// WriteHeader writes the Header record.
func (w *Writer) WriteHeader(h *Header) error {
	data, err := w.enc.Marshal(h)
	if err != nil {
		return err
	}
	_, err = w.bw.Write(data)
	return err
}

// WriteSample writes one Sample record.
func (w *Writer) WriteSample(s *Sample) error {
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
