package pool

// fakepi_test.go contains a minimal in-process fake `pi --mode rpc` for
// unit-testing the worker pool. It is deliberately self-contained (the pool
// package cannot import rpc's unexported test fake) and models only what the
// pool exercises: switch_session/get_state, prompt/agent_start/agent_end,
// abort, and process-exit (stdout close) semantics.
//
// Each fake owns two io.Pipe pairs (Client stdin / Client stdout) and a
// scripted handler invoked per command. The handler can withhold agent_end to
// simulate a still-streaming slot, and emit it later (to test drain-on-reuse).

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
		if !isClosedPipeErr(err) {
			// Best-effort log to stderr; tests tolerate teardown races.
			_ = err
		}
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

// CloseStdoutOnly closes stdout, simulating a pi process exit.
func (f *fakePI) CloseStdoutOnly() { _ = f.fakeStdoutW.Close() }

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
// tests can assert on the commands each slot's process received.
type fakeFactory struct {
	mu     sync.Mutex
	fakes  map[int]*fakePI // indexed by spawn sequence
	envs   map[int]map[string]string
	nextID int
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{fakes: map[int]*fakePI{}, envs: map[int]map[string]string{}}
}

func (ff *fakeFactory) count() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return len(ff.fakes)
}

// fakes returns all spawned fakes keyed by spawn id.
func (ff *fakeFactory) all() map[int]*fakePI {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	out := make(map[int]*fakePI, len(ff.fakes))
	for k, v := range ff.fakes {
		out[k] = v
	}
	return out
}

func (ff *fakeFactory) env(id int) map[string]string {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	out := make(map[string]string, len(ff.envs[id]))
	for k, v := range ff.envs[id] {
		out[k] = v
	}
	return out
}

func (ff *fakeFactory) closeAll() {
	for _, f := range ff.all() {
		f.Close()
	}
}
