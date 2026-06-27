// Package rpc contains the wall-e ↔ pi JSONL RPC protocol layer.
//
// Phase 0 introduces only the framing primitive: a strict JSONL line reader
// that splits records on LF ('\n') only and strips an optional trailing CR.
//
// Per docs/rpc.md ("Framing"), RPC mode uses strict JSONL semantics with LF
// as the only record delimiter. In particular Node's `readline` is NOT
// protocol-compliant because it also splits on U+2028 and U+2029, which are
// valid characters inside a JSON string. This reader deliberately uses a
// hand-rolled index-based scanner so the framing contract is explicit and
// cannot accidentally regress into a "generic line reader" that honors
// Unicode separators.
package rpc

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// ErrLineTooLong is returned by LineReader.ReadFrame when a single record
// exceeds the configured maximum size without a terminating LF.
var ErrLineTooLong = errors.New("rpc: line too long")

// defaultMaxLine is the default cap on a single record. 1 MiB matches the
// Phase 0 test (TestReader_HugeLine) and is far larger than any sane JSONL
// record produced by pi, while still bounding memory if a misbehaving peer
// never emits a newline.
const defaultMaxLine = 1 << 20 // 1 MiB

// LineReader reads JSONL records (one per LF-terminated line) from an
// underlying byte stream.
//
// The returned frame is the record with the trailing line terminator removed.
// If the line ended in "\r\n" the trailing '\r' is also removed, so callers
// receive the exact JSON payload.
//
// LineReader does NOT interpret U+2028 (LINE SEPARATOR) or U+2029
// (PARAGRAPH SEPARATOR) as newlines. Those bytes are returned verbatim as
// part of the current frame, which is required for protocol compliance.
type LineReader struct {
	r       *bufio.Reader
	maxLine int
}

// NewLineReader wraps r with a JSONL line reader using the default max line
// size (1 MiB).
func NewLineReader(r io.Reader) *LineReader {
	return &LineReader{
		r:       bufio.NewReaderSize(r, 64*1024),
		maxLine: defaultMaxLine,
	}
}

// NewLineReaderSize is like NewLineReader but with an explicit maximum line
// size. A record longer than maxLine without a terminating LF yields
// ErrLineTooLong.
func NewLineReaderSize(r io.Reader, maxLine int) *LineReader {
	if maxLine <= 0 {
		maxLine = defaultMaxLine
	}
	return &LineReader{
		r:       bufio.NewReaderSize(r, max(maxLine, 64*1024)),
		maxLine: maxLine,
	}
}

// ReadFrame reads a single JSONL record and returns its raw bytes without
// the line terminator. It returns io.EOF when the underlying stream is
// exhausted with no remaining buffered data.
//
// Implementation notes:
//   - We index for '\n' explicitly inside the buffered slice rather than
//     using bufio.ReadLine, because ReadLine's contract ("isPrefix") is
//     generic line reading and we want the framing rule to be visible and
//     auditable.
//   - A trailing '\r' is stripped so CRLF peers interoperate, but '\r' alone
//     is NOT treated as a separator.
//   - Bytes U+2028 (0xE2 0x80 0xA8) and U+2029 (0xE2 0x80 0xA9) are passed
//     through untouched when they appear inside a record.
func (lr *LineReader) ReadFrame() ([]byte, error) {
	var buf bytes.Buffer

	for {
		// Peek up to as much as is buffered. We scan for '\n' within the
		// peeked window; if not present we consume the window into buf and
		// loop to pull more data from the underlying reader.
		peek, err := lr.r.Peek(lr.r.Buffered())
		// If nothing is buffered, force a read by peeking 1 byte.
		if len(peek) == 0 {
			peek, err = lr.r.Peek(1)
		}

		if len(peek) > 0 {
			nl := bytes.IndexByte(peek, '\n')
			if nl >= 0 {
				// Found a terminator. Append up to (but not including) the LF.
				if _, werr := buf.Write(peek[:nl]); werr != nil {
					return nil, werr
				}
				// Discard the frame bytes plus the LF itself.
				if _, err := lr.r.Discard(nl + 1); err != nil {
					return nil, err
				}
				frame := buf.Bytes()
				// Strip a single trailing CR if present (CRLF tolerance).
				if len(frame) > 0 && frame[len(frame)-1] == '\r' {
					frame = frame[:len(frame)-1]
				}
				return frame, nil
			}

			// No LF in the buffered window; consume it all into buf.
			if _, werr := buf.Write(peek); werr != nil {
				return nil, werr
			}
			if _, err := lr.r.Discard(len(peek)); err != nil {
				return nil, err
			}
			if buf.Len() > lr.maxLine {
				return nil, ErrLineTooLong
			}
			// Loop to pull more.
			continue
		}

		// Nothing to peek. Distinguish EOF from a real error.
		if err != nil {
			if errors.Is(err, io.EOF) {
				if buf.Len() > 0 {
					// Trailing line without a terminator: return it as a
					// final frame (per JSONL common practice). This does not
					// apply to the protocol's frame stream mid-connection,
					// but makes tests and pipe reads well-defined.
					frame := buf.Bytes()
					if len(frame) > 0 && frame[len(frame)-1] == '\r' {
						frame = frame[:len(frame)-1]
					}
					return frame, nil
				}
				return nil, io.EOF
			}
			return nil, err
		}
	}
}
