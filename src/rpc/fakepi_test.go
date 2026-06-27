package rpc

// fakepi_test.go contains a reusable in-process fake `pi --mode rpc` for
// unit-testing Client without spawning a real binary. It speaks the JSONL
// protocol over an io.Pipe pair, driven by a configurable handler.
//
// The fake is deliberately minimal: it is NOT a faithful pi reimplementation.
// It exists to assert Client's framing, id-correlation, event routing, extui
// auto-answer, and process-exit semantics — the things Client is responsible
// for.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakePI owns:
//   - an "in" pipe (Client writes commands here; fake reads from the other end)
//   - an "out" pipe (fake writes events here; Client reads from the other end)
type fakePI struct {
	stdinWriter  *io.PipeWriter // Client writes commands here
	stdoutReader *io.PipeReader // Client reads events here

	// fake-side ends:
	fakeStdinR *bufio.Reader
	fakeStdoutW *io.PipeWriter

	// Lines the fake wrote to stdout, in order (for assertions).
	wroteMu sync.Mutex
	wrote   []string

	// Lines the fake received on stdin (commands + ui responses).
	gotMu sync.Mutex
	got   []string

	stop chan struct{}
	done chan struct{}
}

// newFakePI constructs a fake pi with a fresh pipe pair. Caller must Close()
// it when done.
func newFakePI(t *testing.T) *fakePI {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	f := &fakePI{
		stdinWriter:  inW,
		stdoutReader: outR,
		fakeStdinR:   bufio.NewReader(inR),
		fakeStdoutW:  outW,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	return f
}

// start launches the fake's read loop. The fake reads commands from Client
// and invokes handler for each. handler runs in the read goroutine.
func (f *fakePI) start(handler func(cmd map[string]any)) {
	go func() {
		defer close(f.done)
		for {
			select {
			case <-f.stop:
				return
			default:
			}
			line, err := f.fakeStdinR.ReadString('\n')
			if err != nil {
				return
			}
			f.gotMu.Lock()
			f.got = append(f.got, line)
			f.gotMu.Unlock()

			var cmd map[string]any
			if err := json.Unmarshal([]byte(line), &cmd); err != nil {
				continue
			}
			handler(cmd)
		}
	}()
}

// writeLine writes one JSONL line to the fake's stdout (Client reads it).
// Write errors are tolerated: during teardown the Client may close the read
// end of the pipe while the handler is still emitting trailing events, which
// is harmless.
func (f *fakePI) writeLine(t *testing.T, line string) {
	t.Helper()
	f.wroteMu.Lock()
	f.wrote = append(f.wrote, line)
	f.wroteMu.Unlock()
	if _, err := f.fakeStdoutW.Write([]byte(line + "\n")); err != nil {
		// io.ErrClosedPipe / "closed pipe" races during teardown are expected.
		if !isClosedPipeErr(err) {
			t.Logf("fake stdout write: %v", err)
		}
	}
}

func isClosedPipeErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	return strings.Contains(err.Error(), "closed pipe")
}

// writeJSON marshals v and writes it as a line.
func (f *fakePI) writeJSON(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("fake marshal: %v", err)
	}
	f.writeLine(t, string(b))
}

// Got returns a copy of the lines received on the fake's stdin.
func (f *fakePI) Got() []string {
	f.gotMu.Lock()
	defer f.gotMu.Unlock()
	out := make([]string, len(f.got))
	copy(out, f.got)
	return out
}

// Close stops the fake and closes the pipes. Closing the fake's stdout writer
// causes Client's reader to see EOF → Client reports ErrPiExit.
func (f *fakePI) Close() {
	select {
	case <-f.stop:
	default:
		close(f.stop)
	}
	_ = f.fakeStdoutW.Close()
}

// CloseStdoutOnly closes the fake's stdout, simulating a pi process exit.
func (f *fakePI) CloseStdoutOnly() {
	_ = f.fakeStdoutW.Close()
}
