package chat

// fakepi_test.go contains a minimal in-process fake `pi --mode rpc` for
// unit-testing the chat package. It is deliberately self-contained (the pool /
// rpc / httpapi packages' test fakes are not importable across packages),
// mirroring the pool/httpapi fake pattern: two io.Pipe pairs + a scripted
// handler per slot.
//
// The chat package needs deterministic control over the assistant text deltas a
// turn emits, so the fake supports a "script" mode: on `prompt` it replays a
// list of scripted events (agent_start / text_delta / agent_end) with optional
// inter-event delays, so streaming/edit/steer tests are timing-deterministic.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

// scriptedEvent is one event the fake emits during a turn.
type scriptedEvent struct {
	kind  string // "agent_start", "delta", "agent_end"
	text  string // for "delta"
	delay time.Duration
}

// fakePI owns the pipe ends for one slot's pi process.
type fakePI struct {
	stdinWriter  *io.PipeWriter // Client writes commands here
	stdoutReader *io.PipeReader // Client reads events here

	fakeStdinR  *bufio.Reader // fake reads commands here
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
		// Tolerate closed-pipe races during teardown (handler emitting trailing
		// events while the Client/Pool tears down). Best-effort, non-fatal.
		_ = err
	}
}

func (f *fakePI) writeJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	f.writeLine(string(b))
}

func (f *fakePI) writeResp(id, command string, success bool) {
	f.writeJSON(map[string]any{
		"type":    "response",
		"command": command,
		"success": success,
		"id":      id,
	})
}

// Got returns a copy of the lines received on stdin (commands + ui responses).
func (f *fakePI) Got() []string {
	f.gotMu.Lock()
	defer f.gotMu.Unlock()
	out := make([]string, len(f.got))
	copy(out, f.got)
	return out
}

// contains reports whether any received line contains substr.
func (f *fakePI) contains(substr string) bool {
	for _, l := range f.Got() {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// count reports how many received lines contain substr.
func (f *fakePI) count(substr string) int {
	n := 0
	for _, l := range f.Got() {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// waitForCommand polls until a received line contains substr, or timeout.
func (f *fakePI) waitForCommand(substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f.contains(substr) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return f.contains(substr)
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
// tests can assert on the commands each slot's process received.
type fakeFactory struct {
	mu     sync.Mutex
	fakes  []*fakePI // indexed by spawn sequence
}

func newFakeFactory() *fakeFactory { return &fakeFactory{} }

func (ff *fakeFactory) count() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return len(ff.fakes)
}

// first returns the first spawned fake (most tests have exactly one slot).
func (ff *fakeFactory) first() *fakePI {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if len(ff.fakes) == 0 {
		return nil
	}
	return ff.fakes[0]
}

// waitForFirst polls until at least one fake has been spawned, or timeout.
func (ff *fakeFactory) waitForFirst(timeout time.Duration) *fakePI {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f := ff.first(); f != nil {
			return f
		}
		time.Sleep(2 * time.Millisecond)
	}
	return ff.first()
}

func (ff *fakeFactory) closeAll() {
	ff.mu.Lock()
	all := append([]*fakePI(nil), ff.fakes...)
	ff.mu.Unlock()
	for _, f := range all {
		f.Close()
	}
}

// makeScriptedHandler builds a per-slot handler that replays `script` on each
// prompt. switch_session/get_state/abort/steer are acked. If streamDone is
// non-nil, agent_start is emitted and agent_end waits for streamDone to close
// (overrides script's agent_end timing — used for steer tests that need a
// deliberately-held stream). If script is non-empty, it takes precedence over
// streamDone for emitting agent_end.
func makeScriptedHandler(script []scriptedEvent, streamDone chan struct{}) func(f *fakePI, cmd map[string]any) {
	return func(f *fakePI, cmd map[string]any) {
		id, _ := cmd["id"].(string)
		ctype, _ := cmd["type"].(string)
		switch ctype {
		case "switch_session":
			f.writeResp(id, "switch_session", true)
		case "get_state":
			f.writeJSON(map[string]any{
				"type": "response", "command": "get_state", "success": true, "id": id,
				"data": map[string]any{"sessionFile": "/tmp/test.jsonl", "isStreaming": false},
			})
		case "prompt":
			f.writeResp(id, "prompt", true)
			f.writeJSON(map[string]any{"type": "agent_start"})
			if len(script) > 0 {
				go func() {
					for _, e := range script {
						if e.delay > 0 {
							time.Sleep(e.delay)
						}
						switch e.kind {
						case "agent_start":
							f.writeJSON(map[string]any{"type": "agent_start"})
						case "delta":
							f.writeJSON(map[string]any{
								"type": "message_update",
								"assistantMessageEvent": map[string]any{
									"type":  "text_delta",
									"delta": e.text,
								},
							})
						case "agent_end":
							f.writeJSON(map[string]any{"type": "agent_end"})
						}
					}
				}()
				return
			}
			if streamDone != nil {
				done := streamDone
				go func() { <-done; f.writeJSON(map[string]any{"type": "agent_end"}) }()
				return
			}
			f.writeJSON(map[string]any{"type": "agent_end"})
		case "steer", "follow_up":
			f.writeResp(id, ctype, true)
		case "abort":
			f.writeResp(id, "abort", true)
			f.writeJSON(map[string]any{"type": "agent_end"})
		default:
			f.writeResp(id, ctype, false)
		}
	}
}
