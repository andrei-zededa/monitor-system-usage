package msuformat

import (
	"errors"
	"io"

	"github.com/fxamacker/cbor/v2"
)

// Reader reads CBOR-encoded records from a stream.
type Reader struct {
	dec *cbor.Decoder
}

// NewReader creates a Reader that reads from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{dec: cbor.NewDecoder(r)}
}

// ReadHeader reads the first record as a Header.
func (r *Reader) ReadHeader() (*Header, error) {
	var h Header
	if err := r.dec.Decode(&h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Next reads the next Sample. Returns (nil, nil) at EOF.
// If the stream is truncated mid-record, returns a non-nil error
// but all previously read records are valid.
func (r *Reader) Next() (*Sample, error) {
	var s Sample
	if err := r.dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}
