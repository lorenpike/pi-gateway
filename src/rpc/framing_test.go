package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// wantFrames asserts that reading all frames from r yields exactly want
// (in order). The comparison is on decoded JSON values so that key ordering
// in the payload does not matter; raw bytes are compared when the frame is
// not valid JSON.
func wantFrames(t *testing.T, r *LineReader, want []string) {
	t.Helper()
	got := make([]string, 0, len(want))
	for {
		frame, err := r.ReadFrame()
		if err != nil {
			if err.Error() == "EOF" || err == errEOF {
				break
			}
			// io.EOF comes through here too.
			if isEOF(err) {
				break
			}
			t.Fatalf("unexpected read error: %v", err)
		}
		got = append(got, string(frame))
	}
	if len(got) != len(want) {
		t.Fatalf("frame count mismatch: got %d (%q), want %d (%q)", len(got), got, len(want), want)
	}
	for i := range want {
		if !framesEqual(got[i], want[i]) {
			t.Errorf("frame %d mismatch:\n got: %q\nwant: %q", i, got[i], want[i])
		}
	}
}

// errEOF and isEOF exist so the helper can be reused even before we import
// io in the test file (kept simple here).
var errEOF = newEOF()

func newEOF() error { return &eofError{} }

type eofError struct{}

func (*eofError) Error() string { return "EOF" }

func isEOF(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "EOF")
}

// framesEqual compares two frames. If both are valid JSON objects/arrays, it
// compares decoded values; otherwise it compares the raw bytes. This keeps the
// tests robust against key reordering by encoders.
func framesEqual(a, b string) bool {
	var ja, jb any
	errA := json.Unmarshal([]byte(a), &ja)
	errB := json.Unmarshal([]byte(b), &jb)
	if errA != nil || errB != nil {
		// At least one is not JSON; compare raw.
		return a == b
	}
	ab, _ := json.Marshal(ja)
	bb, _ := json.Marshal(jb)
	return bytes.Equal(ab, bb)
}

// TestReader_SplitsOnLF verifies that LF is the sole record delimiter.
func TestReader_SplitsOnLF(t *testing.T) {
	in := `{"a":1}` + "\n" + `{"b":2}` + "\n"
	r := NewLineReader(strings.NewReader(in))
	wantFrames(t, r, []string{`{"a":1}`, `{"b":2}`})
}

// TestReader_AcceptsCRLF verifies CRLF input yields one record with no
// trailing '\r' in the payload.
func TestReader_AcceptsCRLF(t *testing.T) {
	in := `{"a":1}` + "\r\n"
	r := NewLineReader(strings.NewReader(in))
	wantFrames(t, r, []string{`{"a":1}`})
	// Explicitly assert the payload has no '\r' and a subsequent read hits EOF.
	if _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after one frame, got err %v", err)
	}
}

// TestReader_KeepsUnicodeLineSeparators verifies that U+2028 (LINE SEPARATOR)
// and U+2029 (PARAGRAPH SEPARATOR), encoded in UTF-8 as 0xE2 0x80 0xA8 and
// 0xE2 0x80 0xA9, are NOT treated as line separators when they appear inside
// a JSON string. This is the protocol-compliance requirement called out in
// docs/rpc.md against Node's readline.
func TestReader_KeepsUnicodeLineSeparators(t *testing.T) {
	const sep2028 = "\u2028" // LINE SEPARATOR
	const sep2029 = "\u2029" // PARAGRAPH SEPARATOR

	// A single record whose string value contains both Unicode separators,
	// terminated by a real LF.
	payload := `{"msg":"line1` + sep2028 + `line2` + sep2029 + `end"}`
	in := payload + "\n"
	r := NewLineReader(strings.NewReader(in))

	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame error: %v", err)
	}
	if string(frame) != payload {
		t.Errorf("frame does not contain Unicode separators intact:\n got: %q\nwant: %q", string(frame), payload)
	}

	// Decode and assert the decoded string contains both separators.
	var rec struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(frame, &rec); err != nil {
		t.Fatalf("unmarshal: %v (frame=%q)", err, string(frame))
	}
	if !strings.Contains(rec.Msg, sep2028) {
		t.Errorf("decoded message lost U+2028: %q", rec.Msg)
	}
	if !strings.Contains(rec.Msg, sep2029) {
		t.Errorf("decoded message lost U+2029: %q", rec.Msg)
	}

	// Only one frame should have been produced.
	if _, err := r.ReadFrame(); err == nil {
		t.Errorf("expected EOF after the single record; got another frame (U+2028/U+2029 was treated as a separator)")
	}
}

// TestReader_PartialThenRest verifies that a record delivered across two
// (or more) reads is reassembled into a single frame.
func TestReader_PartialThenRest(t *testing.T) {
	part1 := []byte(`{"a":1,"b":"`)
	part2 := []byte(`hello"}` + "\n")
	r := NewLineReader(&slowReader{chunks: [][]byte{part1, part2}})

	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame error: %v", err)
	}
	want := `{"a":1,"b":"hello"}`
	if string(frame) != want {
		t.Errorf("frame mismatch:\n got: %q\nwant: %q", string(frame), want)
	}
}

// TestReader_HugeLine verifies that a single 1 MiB line is returned as one
// frame (default max is 1 MiB).
func TestReader_HugeLine(t *testing.T) {
	// Default max line is 1 MiB. Build a line whose total length (JSON wrapper
	// included) is exactly at the limit so it is accepted as a single frame.
	overhead := len(`{"x":"`) + len(`"}`) + 1 // wrapper + trailing LF
	size := defaultMaxLine - overhead
	var sb strings.Builder
	sb.WriteString(`{"x":"`)
	for i := 0; i < size; i++ {
		sb.WriteByte('x')
	}
	sb.WriteString(`"}`)
	sb.WriteByte('\n')
	in := sb.String()

	r := NewLineReader(strings.NewReader(in))
	frame, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame error: %v", err)
	}
	var rec struct {
		X string `json:"x"`
	}
	if err := json.Unmarshal(frame, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rec.X) != size {
		t.Errorf("payload length mismatch: got %d, want %d", len(rec.X), size)
	}
}

// slowReader serves fixed byte chunks across successive Read calls, so the
// framing reader must buffer and reassemble.
type slowReader struct {
	chunks [][]byte
	idx    int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.idx >= len(s.chunks) {
		return 0, errEOF
	}
	n := copy(p, s.chunks[s.idx])
	s.idx++
	if n == 0 {
		// chunk empty; advance
		return s.Read(p)
	}
	return n, nil
}
