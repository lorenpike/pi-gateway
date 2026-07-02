// Package rpc contains the wall-e ↔ pi JSONL RPC protocol layer.
//
// client.go implements Client: a Go type that owns a `pi --mode rpc` child
// process and exposes typed command methods plus an Events channel for the
// streaming event/extension-UI sub-protocols.
//
// Concurrency model
// ------------------
// One reader goroutine (run) consumes JSONL frames from pi's stdout using the
// strict LineReader from framing.go. Each frame is decoded once to peek at
// `type`:
//
//   - "response": the request id (if any) is used to look up a pending
//     per-request channel in inflight, and the Response is delivered there.
//     Responses with no id (or an unknown id) are dropped: per docs/rpc.md the
//     caller always supplies an id when it cares about the result, and events
//     after acceptance are reported through the event stream, not as extra
//     responses.
//   - anything else: published as an Event on Events. If it is an
//     extension_ui_request, the extui worker (uiLoop) applies the auto-answer
//     policy and writes the response back to pi's stdin.
//
// The reader goroutine owns stdout; the extui worker owns the framed write to
// stdin (guarded by writeMu). No other goroutine writes to stdin directly;
// command methods go through send(), which takes writeMu.
//
// Lifecycle
// ----------
// New() spawns the process and starts both goroutines. When pi's stdout
// reaches EOF (process exited), run() closes Events and marks the client
// dead; any in-flight request receives ErrPiExit. Close() kills the process
// and waits for run() to exit.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Client owns one `pi --mode rpc` subprocess and the JSONL protocol on top of
// its stdin/stdout. See package docs for the concurrency model.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cfg    Config

	lr *LineReader

	writeMu sync.Mutex

	inflightMu sync.Mutex
	inflight   map[string]chan Response

	eventsMu     sync.Mutex
	eventsCh     chan Event
	eventsClosed bool

	uiCh chan []byte // raw extension_ui_request frames, hand off to uiLoop

	closed  atomic.Bool
	closeMu sync.Mutex
	doneCh  chan struct{} // closed when run() exits

	sessionFile   string
	sessionFileMu sync.RWMutex

	idCounter atomic.Uint64
}

// Config configures a Client. It is a subset of the gateway config focused on
// what the RPC layer needs; the gateway builds it from WALLE_* env vars.
type Config struct {
	// PiBin is the path to the pi executable (default "pi").
	PiBin string
	// Dir is the working directory for the pi process ("" = inherit).
	Dir string
	// Provider is passed via --provider ("" = omit, use pi settings).
	Provider string
	// Model is passed via --model ("" = omit, use pi settings).
	Model string
	// SystemPrompt is passed via --system-prompt ("" = omit, use pi default).
	// pi treats the value as a file path when it exists, otherwise as literal
	// prompt text.
	SystemPrompt string
	// SessionDir is passed via --session-dir ("" = omit, use pi default).
	SessionDir string
	// NoSession passes --no-session (disables persistence). Mutually exclusive
	// with SessionDir; if both set, SessionDir wins.
	NoSession bool

	// UIPolicy is the auto-answer policy for extension-UI dialogs.
	UIPolicy ExtensionUIPolicy

	// RequestTimeout bounds how long a typed command method waits for its
	// Response before returning context.DeadlineExceeded. Zero = no timeout
	// (the caller owns the context). Events keep streaming regardless.
	RequestTimeout time.Duration

	// Stderr, if non-nil, receives pi's stderr (for debugging). If nil,
	// stderr is discarded.
	Stderr io.Writer
}

// Events returns the channel onto which pi's streaming events are published.
// The channel is closed when the pi process exits. Events of type
// "extension_ui_request" are also delivered here *before* being auto-answered;
// consumers that only care about agent progress can ignore them.
func (c *Client) Events() <-chan Event { return c.eventsCh }

// SessionFile returns the most recently observed session file path (updated
// automatically after NewSession/SwitchSession/Clone via GetState resync).
func (c *Client) SessionFile() string {
	c.sessionFileMu.RLock()
	defer c.sessionFileMu.RUnlock()
	return c.sessionFile
}

// New spawns a `pi --mode rpc` process per cfg and starts the reader + extui
// goroutines. The returned Client is ready to issue commands.
func New(cfg Config) (*Client, error) {
	if cfg.PiBin == "" {
		cfg.PiBin = "pi"
	}
	if cfg.UIPolicy == (ExtensionUIPolicy{}) {
		// Zero-value policy would answer confirm=false; default to the plan's
		// confirm=true unless explicitly constructed otherwise. We detect the
		// zero value to apply the documented default.
		cfg.UIPolicy = DefaultExtensionUIPolicy()
	}

	args := []string{"--mode", "rpc"}
	if cfg.Provider != "" {
		args = append(args, "--provider", cfg.Provider)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.SystemPrompt != "" {
		args = append(args, "--system-prompt", cfg.SystemPrompt)
	}
	if cfg.SessionDir != "" {
		args = append(args, "--session-dir", cfg.SessionDir)
	} else if cfg.NoSession {
		args = append(args, "--no-session")
	}

	cmd := exec.Command(cfg.PiBin, args...)
	cmd.Dir = cfg.Dir
	if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	} else {
		cmd.Stderr = io.Discard
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("rpc: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("rpc: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("rpc: start %s: %w", cfg.PiBin, err)
	}

	c := &Client{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		cfg:      cfg,
		lr:       NewLineReader(stdout),
		inflight: make(map[string]chan Response),
		eventsCh: make(chan Event, 64),
		uiCh:     make(chan []byte, 16),
		doneCh:   make(chan struct{}),
	}

	go c.run()
	go c.uiLoop()

	return c, nil
}

// run is the single reader goroutine. It owns stdout/LineReader consumption.
func (c *Client) run() {
	defer close(c.doneCh)
	defer c.closeEvents()

	// Drain the ui handoff channel after events close so uiLoop can exit.
	defer close(c.uiCh)

	for {
		frame, err := c.lr.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.failInflight(ErrPiExit)
				return
			}
			// A framing error is effectively the same as losing the stream.
			c.failInflight(fmt.Errorf("%w: framing: %v", ErrPiExit, err))
			return
		}

		// Peek at `type` without forcing full decode.
		var head struct {
			Type string `json:"type"`
			ID   string `json:"id,omitempty"`
		}
		if err := json.Unmarshal(frame, &head); err != nil {
			// Unparseable line: surface as an event so consumers can log it.
			c.publish(Event{Type: "parse_error", Raw: frame})
			continue
		}

		switch head.Type {
		case "response":
			var resp Response
			if err := json.Unmarshal(frame, &resp); err != nil {
				// Shouldn't happen for a real pi; treat as stream loss.
				c.publish(Event{Type: "parse_error", Raw: frame})
				continue
			}
			c.deliverResponse(resp)
		case "extension_ui_request":
			// Publish to consumers AND hand off to the extui worker for
			// auto-answering. Copy the frame so uiLoop can outlive the
			// events channel consumer.
			cp := make([]byte, len(frame))
			copy(cp, frame)
			c.publish(Event{Type: head.Type, Raw: frame})
			select {
			case c.uiCh <- cp:
			default:
				// extui worker is slow/full; drop auto-answer rather than
				// blocking the reader. The agent will time out the dialog.
			}
		default:
			c.publish(Event{Type: head.Type, Raw: frame})
		}
	}
}

// uiLoop is the extui worker goroutine. It owns stdin writes for
// extension_ui_response envelopes only (command writes go through send()).
func (c *Client) uiLoop() {
	for frame := range c.uiCh {
		out, err := c.cfg.UIPolicy.BuildUIResponse(frame)
		if err != nil || len(out) == 0 {
			continue
		}
		if err := c.writeFrame(out); err != nil {
			// Process is dying or dead; nothing useful to do.
			return
		}
	}
}

// publish delivers an event to Events, non-blocking; if the channel is full it
// blocks (callers should be draining). After closeEvents is called, publish is
// a no-op.
func (c *Client) publish(ev Event) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	if c.eventsClosed {
		return
	}
	c.eventsCh <- ev
}

func (c *Client) closeEvents() {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	c.eventsClosed = true
	close(c.eventsCh)
}

// deliverResponse routes a Response to the waiting caller, if any.
func (c *Client) deliverResponse(resp Response) {
	c.inflightMu.Lock()
	ch, ok := c.inflight[resp.ID]
	if ok {
		delete(c.inflight, resp.ID)
	}
	c.inflightMu.Unlock()
	if !ok {
		return
	}
	// Non-blocking send: every request slot is buffered(1), so this cannot
	// block the reader even if the caller has given up.
	ch <- resp
}

// failInflight unblocks all waiting callers with err.
func (c *Client) failInflight(err error) {
	c.inflightMu.Lock()
	defer c.inflightMu.Unlock()
	for id, ch := range c.inflight {
		// We can't deliver `err` on a Response channel; instead close the
		// channel so callers see a zero Response + ctx-aware error. To keep
		// the API simple, we close the channel and rely on callers to also
		// watch doneCh/ErrPiExit. For typed methods we additionally watch
		// doneCh.
		_ = err
		close(ch)
		delete(c.inflight, id)
	}
}

// nextID returns a monotonically increasing request id string.
func (c *Client) nextID() string {
	return fmt.Sprintf("req-%d", c.idCounter.Add(1))
}

// writeFrame writes one JSONL record (no trailing newline expected in b) to
// pi's stdin under the write mutex.
func (c *Client) writeFrame(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(b); err != nil {
		return err
	}
	if _, err := c.stdin.Write([]byte("\n")); err != nil {
		return err
	}
	return nil
}

// send sends a command and waits for its Response. The command must include
// only marshalable fields; an `id` is injected automatically. On process
// exit before the response arrives, send returns ErrPiExit.
func (c *Client) send(ctx context.Context, cmd map[string]any) (Response, error) {
	id := c.nextID()
	cmd["id"] = id
	cmd["type"] = cmd["type"] // caller sets; keep as-is

	buf, err := json.Marshal(cmd)
	if err != nil {
		return Response{}, fmt.Errorf("rpc: marshal: %w", err)
	}

	ch := make(chan Response, 1)
	c.inflightMu.Lock()
	if c.closed.Load() {
		c.inflightMu.Unlock()
		return Response{}, ErrPiExit
	}
	c.inflight[id] = ch
	c.inflightMu.Unlock()

	// Ensure we clean up the slot on all exit paths.
	defer func() {
		c.inflightMu.Lock()
		if _, ok := c.inflight[id]; ok {
			delete(c.inflight, id)
		}
		c.inflightMu.Unlock()
	}()

	if err := c.writeFrame(buf); err != nil {
		return Response{}, fmt.Errorf("rpc: write: %w", err)
	}

	timeout := c.cfg.RequestTimeout
	var timer *time.Timer
	var timerCh <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timerCh = timer.C
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			// Channel closed = process exited / failInflight.
			return Response{}, ErrPiExit
		}
		return resp, nil
	case <-timerCh:
		return Response{}, context.DeadlineExceeded
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-c.doneCh:
		return Response{}, ErrPiExit
	}
}

// NewClientFromStreams constructs a Client over already-open streams
// (stdin/stdout pipes) without spawning a process. It is intended for advanced
// in-process transports and for tests with a fake pi; production code uses
// New(). The stdin must be a write end (Client writes commands to it) and
// stdout a read end (Client reads events from it). Both are closed by Close.
func NewClientFromStreams(stdin io.WriteCloser, stdout io.ReadCloser, cfg Config) *Client {
	if cfg.UIPolicy == (ExtensionUIPolicy{}) {
		cfg.UIPolicy = DefaultExtensionUIPolicy()
	}
	c := &Client{
		stdin:    stdin,
		stdout:   stdout,
		cfg:      cfg,
		lr:       NewLineReader(stdout),
		inflight: make(map[string]chan Response),
		eventsCh: make(chan Event, 64),
		uiCh:     make(chan []byte, 16),
		doneCh:   make(chan struct{}),
		// cmd is nil; Close() must handle that.
	}
	go c.run()
	go c.uiLoop()
	return c
}

// Close terminates the pi process and waits for the reader goroutine to exit.
// It is safe to call multiple times.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Kill the process; ignore "already dead" errors. In the test path cmd
	// is nil and we just close the streams.
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	// Close stdin (signals EOF to the child/fake read loop) and stdout (the
	// read end) so the reader goroutine unblocks and run() can exit. Killing
	// the process normally closes its stdout for us; closing it explicitly
	// covers the test path where there is no process to kill.
	_ = c.stdin.Close()
	_ = c.stdout.Close()
	<-c.doneCh
	if c.cmd != nil {
		_ = c.cmd.Wait()
	}
	return nil
}

// --- Typed command methods ------------------------------------------------

// Prompt sends a `prompt` command. If steer is true, streamingBehavior is set
// to "steer" (required when the agent is already streaming).
func (c *Client) Prompt(ctx context.Context, message string, steer bool) (Response, error) {
	cmd := map[string]any{"type": "prompt", "message": message}
	if steer {
		cmd["streamingBehavior"] = "steer"
	}
	return c.send(ctx, cmd)
}

// Steer sends a `steer` command (queue while streaming).
func (c *Client) Steer(ctx context.Context, message string) (Response, error) {
	return c.send(ctx, map[string]any{"type": "steer", "message": message})
}

// FollowUp sends a `follow_up` command (deliver when agent stops).
func (c *Client) FollowUp(ctx context.Context, message string) (Response, error) {
	return c.send(ctx, map[string]any{"type": "follow_up", "message": message})
}

// Abort sends an `abort` command.
func (c *Client) Abort(ctx context.Context) (Response, error) {
	return c.send(ctx, map[string]any{"type": "abort"})
}

// NewSession sends a `new_session` command and then automatically resyncs the
// known sessionFile via GetState. Returns the final Response (from new_session)
// and the resolved State.
func (c *Client) NewSession(ctx context.Context) (Response, State, error) {
	resp, err := c.send(ctx, map[string]any{"type": "new_session"})
	if err != nil {
		return resp, State{}, err
	}
	if !resp.Success {
		return resp, State{}, nil
	}
	// Don't fail the whole call if resync fails; just leave the file stale.
	_, _ = c.GetState(ctx)
	return resp, c.state(), nil
}

// SwitchSession sends a `switch_session` command and resyncs sessionFile.
func (c *Client) SwitchSession(ctx context.Context, path string) (Response, State, error) {
	resp, err := c.send(ctx, map[string]any{"type": "switch_session", "sessionPath": path})
	if err != nil {
		return resp, State{}, err
	}
	_, _ = c.GetState(ctx)
	return resp, c.state(), nil
}

// Clone sends a `clone` command and resyncs sessionFile.
func (c *Client) Clone(ctx context.Context) (Response, State, error) {
	resp, err := c.send(ctx, map[string]any{"type": "clone"})
	if err != nil {
		return resp, State{}, err
	}
	_, _ = c.GetState(ctx)
	return resp, c.state(), nil
}

// GetState fetches current session state and updates the cached sessionFile.
func (c *Client) GetState(ctx context.Context) (State, error) {
	resp, err := c.send(ctx, map[string]any{"type": "get_state"})
	if err != nil {
		return State{}, err
	}
	if !resp.Success {
		return State{}, fmt.Errorf("rpc: get_state failed: %s", resp.Error)
	}
	var st State
	if err := json.Unmarshal(resp.Data, &st); err != nil {
		return State{}, fmt.Errorf("rpc: decode state: %w", err)
	}
	c.sessionFileMu.Lock()
	c.sessionFile = st.SessionFile
	c.sessionFileMu.Unlock()
	return st, nil
}

// GetMessages sends a `get_messages` command and returns the raw messages
// payload (decoded as json.RawMessage for caller flexibility).
func (c *Client) GetMessages(ctx context.Context) (json.RawMessage, error) {
	resp, err := c.send(ctx, map[string]any{"type": "get_messages"})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("rpc: get_messages failed: %s", resp.Error)
	}
	var out struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return nil, fmt.Errorf("rpc: decode messages: %w", err)
	}
	return out.Messages, nil
}

// Compact sends a `compact` command. customInstructions is optional.
func (c *Client) Compact(ctx context.Context, customInstructions string) (Response, error) {
	cmd := map[string]any{"type": "compact"}
	if customInstructions != "" {
		cmd["customInstructions"] = customInstructions
	}
	return c.send(ctx, cmd)
}

// Bash sends a `bash` command.
func (c *Client) Bash(ctx context.Context, command string) (Response, error) {
	return c.send(ctx, map[string]any{"type": "bash", "command": command})
}

// GetLastAssistantText sends `get_last_assistant_text`.
func (c *Client) GetLastAssistantText(ctx context.Context) (string, error) {
	resp, err := c.send(ctx, map[string]any{"type": "get_last_assistant_text"})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		return "", fmt.Errorf("rpc: get_last_assistant_text failed: %s", resp.Error)
	}
	var out struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return "", fmt.Errorf("rpc: decode last text: %w", err)
	}
	if out.Text == nil {
		return "", nil
	}
	return *out.Text, nil
}

// ExportHTML sends `export_html` and returns the path pi wrote.
func (c *Client) ExportHTML(ctx context.Context, outputPath string) (string, error) {
	resp, err := c.send(ctx, map[string]any{"type": "export_html", "outputPath": outputPath})
	if err != nil {
		return "", err
	}
	if !resp.Success {
		return "", fmt.Errorf("rpc: export_html failed: %s", resp.Error)
	}
	var out struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return "", fmt.Errorf("rpc: decode export_html: %w", err)
	}
	return out.Path, nil
}

// GetSessionStats sends `get_session_stats`.
func (c *Client) GetSessionStats(ctx context.Context) (json.RawMessage, error) {
	resp, err := c.send(ctx, map[string]any{"type": "get_session_stats"})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("rpc: get_session_stats failed: %s", resp.Error)
	}
	return resp.Data, nil
}

// GetCommands sends `get_commands`.
func (c *Client) GetCommands(ctx context.Context) (json.RawMessage, error) {
	resp, err := c.send(ctx, map[string]any{"type": "get_commands"})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("rpc: get_commands failed: %s", resp.Error)
	}
	return resp.Data, nil
}

// SetModel sends `set_model`.
func (c *Client) SetModel(ctx context.Context, provider, modelID string) (Response, error) {
	return c.send(ctx, map[string]any{
		"type":     "set_model",
		"provider": provider,
		"modelId":  modelID,
	})
}

// state returns a snapshot of the cached sessionFile-derived state.
func (c *Client) state() State {
	sf := c.SessionFile()
	return State{SessionFile: sf}
}

// Wait blocks until the pi process and reader goroutine have exited. It
// returns the process error (if any). Useful for detached lifetime tests.
func (c *Client) Wait() error {
	<-c.doneCh
	return c.cmd.Wait()
}
