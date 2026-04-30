package msuformat

import (
	"errors"
	"fmt"
	"io"

	"github.com/fxamacker/cbor/v2"
)

// ErrUnsupportedVersion is returned when the reader encounters a file
// whose Header.V does not match FormatVersion.
var ErrUnsupportedVersion = errors.New("unsupported MSU format version")

// envelope is decoded first from each record; it carries just enough to
// dispatch to the concrete record type.
type envelope struct {
	V    int    `cbor:"v"`
	Type string `cbor:"type"`
}

// Reader reads CBOR-encoded records from a stream. It transparently
// handles the v2 source dictionary: SourceDef records populate an
// internal id→tuple map, and Samples returned from Next() have their
// Section/Cmd/NS fields reconstructed from that map.
type Reader struct {
	dec  *cbor.Decoder
	srcs map[uint16]SourceDef

	// OnHeader, if non-nil, is called when Next() encounters a second (or
	// later) Header record mid-stream — this happens when msu-collect is
	// run multiple times against the same output file (writer uses
	// O_APPEND, so each run concatenates). The source dictionary is reset
	// and reading continues transparently.
	OnHeader func(*Header)
}

// NewReader creates a Reader that reads from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{
		dec:  cbor.NewDecoder(r),
		srcs: make(map[uint16]SourceDef),
	}
}

// ReadHeader reads the first record as a Header. It returns
// ErrUnsupportedVersion (wrapped, with version numbers) if the file's
// format version doesn't match this binary's FormatVersion.
func (r *Reader) ReadHeader() (*Header, error) {
	var raw cbor.RawMessage
	if err := r.dec.Decode(&raw); err != nil {
		return nil, err
	}

	var env envelope
	if err := cbor.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decoding header envelope: %w", err)
	}
	if env.Type != TypeHeader {
		return nil, fmt.Errorf("expected header record, got type %q", env.Type)
	}
	if env.V != FormatVersion {
		return nil, fmt.Errorf("%w: file is v%d, this binary supports v%d",
			ErrUnsupportedVersion, env.V, FormatVersion)
	}

	var h Header
	if err := cbor.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Next reads the next Sample from the stream, transparently consuming
// any SourceDef records along the way. Returns (nil, nil) at EOF or
// on a truncated trailing record (partial write from a crash).
func (r *Reader) Next() (*Sample, error) {
	for {
		var raw cbor.RawMessage
		if err := r.dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, nil
			}
			return nil, err
		}

		var env envelope
		if err := cbor.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("decoding record envelope: %w", err)
		}

		switch env.Type {
		case TypeSourceDef:
			var def SourceDef
			if err := cbor.Unmarshal(raw, &def); err != nil {
				return nil, fmt.Errorf("decoding source definition: %w", err)
			}
			r.srcs[def.ID] = def
			continue

		case TypeSample:
			var s Sample
			if err := cbor.Unmarshal(raw, &s); err != nil {
				return nil, fmt.Errorf("decoding sample: %w", err)
			}
			def, ok := r.srcs[s.SrcID]
			if !ok {
				return nil, fmt.Errorf("sample references unknown source id %d", s.SrcID)
			}
			s.Section = def.Section
			s.Cmd = def.Cmd
			s.NS = def.NS
			return &s, nil

		case TypeHeader:
			var h Header
			if err := cbor.Unmarshal(raw, &h); err != nil {
				return nil, fmt.Errorf("decoding mid-stream header: %w", err)
			}
			if h.V != FormatVersion {
				return nil, fmt.Errorf("%w: mid-stream header is v%d, this binary supports v%d",
					ErrUnsupportedVersion, h.V, FormatVersion)
			}
			r.srcs = make(map[uint16]SourceDef)
			if r.OnHeader != nil {
				r.OnHeader(&h)
			}
			continue

		default:
			return nil, fmt.Errorf("unknown record type %q", env.Type)
		}
	}
}
