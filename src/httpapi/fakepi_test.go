package httpapi

// fakepi_test.go contains a minimal in-process fake `pi --mode rpc` for
// unit-testing the HTTP API against a real pool wired with fakes. It is
// self-contained (the rpc and pool packages' test fakes are not importable
// from httpapi), and models the events the SSE layer consumes:
// switch_session/get_state, prompt (agent_start + message_update text_delta +
// agent_end), abort, and process-exit (stdout close).
//
// Each fake owns two io.Pipe pairs (Client stdin / Client stdout) and a
// scripted handler invoked per command. The handler can withhold agent_end to
// simulate a still-streaming slot, emitting it on abort (used by the
// client-disconnect test).

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// fakePI owns the pipe ends for one slot's pi process.
type fakePI struct {
	stdinWriter  *io.PipeWriter // Client writes commands here
	stdoutReader *io.PipeReader // Client reads events here

	fakeStdinR  *bufio.Reader  // fake reads commands here
	fakeStdoutW *io.PipeWriter // fake writes events here

	gotMu sync.Mutex
	got   []string

	stop chan struct{}
	done chan struct{}
}

func newFakePI() *fakePI {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return &fakePI{
		stdinWriter:  inW,
		stdoutReader: outR,
		fakeStdinR:   bufio.NewReader(inR),
		fakeStdoutW:  outW,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
}

// start launches the read loop. handler runs in the read goroutine.
func (f *fakePI) start(handler func(f *fakePI, cmd map[string]any)) {
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
			handler(f, cmd)
		}
	}()
}

func (f *fakePI) writeLine(line string) {
	if _, err := f.fakeStdoutW.Write([]byte(line + "\n")); err != nil {
		// Tolerate closed pipe on teardown.
		_ = isClosedPipeErr(err)
	}
}

func (f *fakePI) writeJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	f.writeLine(string(b))
}

func (f *fakePI) writeResp(id, command string, success bool, data map[string]any) {
	out := map[string]any{
		"type": "response", "command": command, "success": success, "id": id,
	}
	for k, v := range data {
		out[k] = v
	}
	f.writeJSON(out)
}

// emitAssistantText writes a sequence: agent_start, two text_delta events,
// then agent_end, simulating a normal streaming turn whose deltas concatenate
// to `text`.
func (f *fakePI) emitAssistantText(prefix, suffix string) {
	f.writeJSON(map[string]any{"type": "agent_start"})
	f.writeJSON(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":         "text_delta",
			"contentIndex": 0,
			"delta":        prefix,
		},
	})
	if suffix != "" {
		f.writeJSON(map[string]any{
			"type": "message_update",
			"assistantMessageEvent": map[string]any{
				"type":         "text_delta",
				"contentIndex": 0,
				"delta":        suffix,
			},
		})
	}
	f.writeJSON(map[string]any{"type": "agent_end"})
}

// Got returns a copy of the lines received on stdin (commands + ui responses).
func (f *fakePI) Got() []string {
	f.gotMu.Lock()
	defer f.gotMu.Unlock()
	out := make([]string, len(f.got))
	copy(out, f.got)
	return out
}

// Contains reports whether any received line contains substr.
func (f *fakePI) Contains(substr string) bool {
	for _, l := range f.Got() {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// waitForCommand polls until a received line contains substr, or timeout.
func (f *fakePI) waitForCommand(substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.Contains(substr) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return f.Contains(substr)
}

func (f *fakePI) Close() {
	select {
	case <-f.stop:
	default:
		close(f.stop)
	}
	_ = f.fakeStdoutW.Close()
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

// --- fake-driven pool factory ---------------------------------------------

// fakeFactory builds rpc.Clients over fresh fake pis and records each fake so
// tests can assert on the commands each slot's process received (e.g. abort
// on client disconnect).
type fakeFactory struct {
	mu     sync.Mutex
	fakes  map[int]*fakePI
	nextID int
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{fakes: map[int]*fakePI{}}
}

func (ff *fakeFactory) count() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return len(ff.fakes)
}

func (ff *fakeFactory) all() map[int]*fakePI {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	out := make(map[int]*fakePI, len(ff.fakes))
	for k, v := range ff.fakes {
		out[k] = v
	}
	return out
}

func (ff *fakeFactory) closeAll() {
	for _, f := range ff.all() {
		f.Close()
	}
}
