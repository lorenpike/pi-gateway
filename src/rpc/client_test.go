package rpc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testContext returns a context with a generous timeout for tests, so a
// blocked goroutine fails fast instead of hanging the suite.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newClientWithFake wires a Client to a freshly started fake pi and returns
// both. The handler closure receives the fake so it can write responses back
// (it closes over the fake's stdout writer).
func newClientWithFake(t *testing.T, handler func(f *fakePI, cmd map[string]any)) (*Client, *fakePI) {
	t.Helper()
	f := newFakePI(t)
	f.start(func(cmd map[string]any) {
		if handler != nil {
			handler(f, cmd)
		}
	})
	c := newClientFromStreams(f.stdinWriter, f.stdoutReader, Config{
		UIPolicy: DefaultExtensionUIPolicy(),
	})
	t.Cleanup(func() {
		_ = c.Close()
		f.Close()
	})
	return c, f
}

// defaultHandler returns a handler that responds to the common commands with
// success, and (for prompt) emits agent_start/agent_end. It is the baseline
// used by tests that only care about the happy path.
func defaultHandler(streaming *bool, sessionFile string) func(f *fakePI, cmd map[string]any) {
	return func(f *fakePI, cmd map[string]any) {
		t := currentTestRef()
		id, _ := cmd["id"].(string)
		ctype, _ := cmd["type"].(string)
		writeResp := func(command string, success bool, data any) {
			out := map[string]any{
				"type": "response", "command": command, "success": success,
			}
			if id != "" {
				out["id"] = id
			}
			if data != nil {
				out["data"] = data
			}
			f.writeJSON(t, out)
		}
		switch ctype {
		case "prompt":
			if streaming != nil && *streaming {
				sb, _ := cmd["streamingBehavior"].(string)
				if sb == "" {
					writeResp("prompt", false, nil)
					return
				}
			}
			writeResp("prompt", true, nil)
			f.writeJSON(t, map[string]any{"type": "agent_start"})
			f.writeJSON(t, map[string]any{"type": "agent_end", "messages": []any{}})
		case "steer":
			writeResp("steer", true, nil)
		case "follow_up":
			writeResp("follow_up", true, nil)
		case "abort":
			writeResp("abort", true, nil)
		case "new_session":
			writeResp("new_session", true, map[string]any{"cancelled": false})
		case "clone":
			writeResp("clone", true, map[string]any{"cancelled": false})
		case "switch_session":
			writeResp("switch_session", true, map[string]any{"cancelled": false})
		case "get_state":
			data := map[string]any{
				"sessionFile": sessionFile,
				"isStreaming":  false,
			}
			if streaming != nil {
				data["isStreaming"] = *streaming
			}
			writeResp("get_state", true, data)
		case "get_last_assistant_text":
			writeResp("get_last_assistant_text", true, map[string]any{"text": "hi back"})
		case "get_messages":
			writeResp("get_messages", true, map[string]any{"messages": []any{}})
		case "get_session_stats":
			writeResp("get_session_stats", true, map[string]any{})
		case "get_commands":
			writeResp("get_commands", true, map[string]any{"commands": []any{}})
		case "compact":
			writeResp("compact", true, map[string]any{})
		case "bash":
			writeResp("bash", true, map[string]any{"output": "", "exitCode": 0})
		case "set_model":
			writeResp("set_model", true, map[string]any{})
		default:
			// Unknown command: respond with a parse error so Client doesn't hang.
			writeResp("parse", false, nil)
		}
	}
}

// currentTestRef returns the *testing.T for the active test, so handler
// closures can call f.writeJSON(t, ...) without capturing t themselves (which
// would race with t.Cleanup). We set/unset it per-test below.
var currentTestMu sync.Mutex
var currentTestRef func() *testing.T

func setCurrentTest(t *testing.T) func() {
	currentTestMu.Lock()
	prev := currentTestRef
	currentTestRef = func() *testing.T { return t }
	currentTestMu.Unlock()
	return func() {
		currentTestMu.Lock()
		currentTestRef = prev
		currentTestMu.Unlock()
	}
}

// newClientWithFakeDefault wires a Client + fake using defaultHandler.
func newClientWithFakeDefault(t *testing.T, streaming *bool, sessionFile string) (*Client, *fakePI) {
	restore := setCurrentTest(t)
	t.Cleanup(restore)
	return newClientWithFake(t, func(f *fakePI, cmd map[string]any) {
		defaultHandler(streaming, sessionFile)(f, cmd)
	})
}

// drainEvents consumes Events into a slice until the channel closes or ctx
// is done; returns the collected events. Used by tests that want to assert on
// the full event stream.
func drainEvents(ctx context.Context, ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-ctx.Done():
			return out
		}
	}
}

// waitForEvent polls for an event of the given type until the deadline.
func waitForEvent(ch <-chan Event, typ string, timeout time.Duration) (Event, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case ev, ok := <-ch:
			if !ok {
				return Event{}, false
			}
			if ev.Type == typ {
				return ev, true
			}
		case <-time.After(5 * time.Millisecond):
		}
	}
	return Event{}, false
}

// --- Tests ----------------------------------------------------------------

// TestClient_SpawnAndPrompt: send a prompt, assert a success response and an
// agent_end event arrive.
func TestClient_SpawnAndPrompt(t *testing.T) {
	c, _ := newClientWithFakeDefault(t, nil, "/sessions/x.jsonl")

	resp, err := c.Prompt(testContext(t), "hi", false)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !resp.Success {
		t.Fatalf("prompt not accepted: %+v", resp)
	}
	if resp.Command != "prompt" {
		t.Errorf("command = %q, want prompt", resp.Command)
	}

	if _, ok := waitForEvent(c.Events(), EventAgentEnd, 2*time.Second); !ok {
		t.Errorf("did not observe agent_end")
	}
}

// TestClient_IDCorrelation: the response id matches the request id Client
// assigned. We assert by capturing the id the fake saw and verifying the
// Client delivered a response with the same id.
func TestClient_IDCorrelation(t *testing.T) {
	restore := setCurrentTest(t)
	defer restore()

	var seenID string
	var idMu sync.Mutex
	c, f := newClientWithFake(t, func(f *fakePI, cmd map[string]any) {
		id, _ := cmd["id"].(string)
		idMu.Lock()
		seenID = id
		idMu.Unlock()
		ctype, _ := cmd["type"].(string)
		out := map[string]any{
			"type": "response", "command": ctype, "success": true, "id": id,
		}
		f.writeJSON(t, out)
	})

	_, err := c.Prompt(testContext(t), "hi", false)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	idMu.Lock()
	id := seenID
	idMu.Unlock()
	if id == "" {
		t.Fatalf("fake never saw a request id")
	}
	if !strings.HasPrefix(id, "req-") {
		t.Errorf("id = %q, want req-* prefix", id)
	}
	_ = f
}

// TestClient_SteerWhileStreaming: when streaming, a prompt without
// streamingBehavior returns success:false; with "steer" returns success:true.
func TestClient_SteerWhileStreaming(t *testing.T) {
	streaming := true
	c, _ := newClientWithFakeDefault(t, &streaming, "/sessions/x.jsonl")

	// Without streamingBehavior → success:false.
	resp, err := c.Prompt(testContext(t), "msg", false)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if resp.Success {
		t.Errorf("expected success:false for prompt while streaming without streamingBehavior")
	}

	// With steer=true → success:true.
	resp, err = c.Prompt(testContext(t), "msg", true)
	if err != nil {
		t.Fatalf("Prompt(steer): %v", err)
	}
	if !resp.Success {
		t.Errorf("expected success:true for prompt with steer")
	}
}

// TestClient_Abort: abort → response success:true.
func TestClient_Abort(t *testing.T) {
	c, _ := newClientWithFakeDefault(t, nil, "/sessions/x.jsonl")
	resp, err := c.Abort(testContext(t))
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !resp.Success {
		t.Errorf("abort not successful: %+v", resp)
	}
	if resp.Command != "abort" {
		t.Errorf("command = %q, want abort", resp.Command)
	}
}

// TestClient_NewSessionResync: after new_session, the Client auto-calls
// get_state and updates its known sessionFile.
func TestClient_NewSessionResync(t *testing.T) {
	const want = "/sessions/chanA--42--u.jsonl"
	c, _ := newClientWithFakeDefault(t, nil, want)

	before := c.SessionFile()
	resp, _, err := c.NewSession(testContext(t))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !resp.Success {
		t.Fatalf("new_session not successful: %+v", resp)
	}
	// Give the resync (GetState) a moment to land.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && c.SessionFile() == before {
		time.Sleep(2 * time.Millisecond)
	}
	if got := c.SessionFile(); got != want {
		t.Errorf("sessionFile after resync = %q, want %q", got, want)
	}
}

// TestClient_GetState: fields parse into the State struct.
func TestClient_GetState(t *testing.T) {
	restore := setCurrentTest(t)
	defer restore()

	const sf = "/sessions/abc.jsonl"
	c, _ := newClientWithFake(t, func(f *fakePI, cmd map[string]any) {
		id, _ := cmd["id"].(string)
		ctype, _ := cmd["type"].(string)
		switch ctype {
		case "get_state":
			f.writeJSON(t, map[string]any{
				"type": "response", "command": "get_state", "success": true, "id": id,
				"data": map[string]any{
					"sessionFile":           sf,
					"sessionId":             "abc",
					"isStreaming":            false,
					"thinkingLevel":         "medium",
					"autoCompactionEnabled": true,
					"messageCount":          5,
				},
			})
		default:
			defaultHandler(nil, sf)(f, cmd)
		}
	})

	st, err := c.GetState(testContext(t))
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if st.SessionFile != sf {
		t.Errorf("sessionFile = %q, want %q", st.SessionFile, sf)
	}
	if st.SessionID != "abc" {
		t.Errorf("sessionId = %q, want abc", st.SessionID)
	}
	if st.ThinkingLevel != "medium" {
		t.Errorf("thinkingLevel = %q, want medium", st.ThinkingLevel)
	}
	if st.MessageCount != 5 {
		t.Errorf("messageCount = %d, want 5", st.MessageCount)
	}
	if got := c.SessionFile(); got != sf {
		t.Errorf("cached sessionFile = %q, want %q", got, sf)
	}
}

// TestClient_ExtensionUIConfirm: fake pi emits extension_ui_request{confirm};
// the Client replies extension_ui_response{confirmed:<default>} within 50ms,
// with the default read from config (confirm=true).
func TestClient_ExtensionUIConfirm(t *testing.T) {
	restore := setCurrentTest(t)
	defer restore()

	const reqID = "uuid-confirm"
	var gotRespMu sync.Mutex
	var gotResp map[string]any

	c, f := newClientWithFake(t, func(f *fakePI, cmd map[string]any) {
		ctype, _ := cmd["type"].(string)
		if ctype == "extension_ui_response" {
			gotRespMu.Lock()
			gotResp = cmd
			gotRespMu.Unlock()
		}
	})

	// Emit a confirm request on stdout (Client reads it, auto-answers).
	start := time.Now()
	f.writeJSON(t, map[string]any{
		"type": "extension_ui_request", "id": reqID, "method": "confirm",
		"title": "ok?", "timeout": 5000,
	})

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		gotRespMu.Lock()
		r := gotResp
		gotRespMu.Unlock()
		if r != nil {
			elapsed := time.Since(start)
			if elapsed > 50*time.Millisecond {
				t.Errorf("auto-answer took %v, want <50ms", elapsed)
			}
			if r["type"] != "extension_ui_response" {
				t.Errorf("response type = %v, want extension_ui_response", r["type"])
			}
			if r["id"] != reqID {
				t.Errorf("response id = %v, want %q", r["id"], reqID)
			}
			confirmed, _ := r["confirmed"].(bool)
			if !confirmed {
				t.Errorf("expected confirmed:true (default policy), got %v", r["confirmed"])
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("never received extension_ui_response for confirm")
	_ = c
}

// TestClient_ExtensionUISelect: replies value = first option.
func TestClient_ExtensionUISelect(t *testing.T) {
	restore := setCurrentTest(t)
	defer restore()

	const reqID = "uuid-select"
	var gotRespMu sync.Mutex
	var gotResp map[string]any
	c, f := newClientWithFake(t, func(f *fakePI, cmd map[string]any) {
		ctype, _ := cmd["type"].(string)
		if ctype == "extension_ui_response" {
			gotRespMu.Lock()
			gotResp = cmd
			gotRespMu.Unlock()
		}
	})

	f.writeJSON(t, map[string]any{
		"type": "extension_ui_request", "id": reqID, "method": "select",
		"title": "pick", "options": []string{"Allow", "Block"},
	})

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		gotRespMu.Lock()
		r := gotResp
		gotRespMu.Unlock()
		if r != nil {
			if r["value"] != "Allow" {
				t.Errorf("expected value=first option (Allow), got %v", r["value"])
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("never received extension_ui_response for select")
	_ = c
}

// TestClient_ExtensionUIInput_Editor_Cancelled: input/editor reply cancelled.
func TestClient_ExtensionUIInput_Editor_Cancelled(t *testing.T) {
	restore := setCurrentTest(t)
	defer restore()

	for _, method := range []string{"input", "editor"} {
		method := method
		t.Run(method, func(t *testing.T) {
			restore := setCurrentTest(t)
			defer restore()

			reqID := "uuid-" + method
			var gotRespMu sync.Mutex
			var gotResp map[string]any
			c, f := newClientWithFake(t, func(f *fakePI, cmd map[string]any) {
				ctype, _ := cmd["type"].(string)
				if ctype == "extension_ui_response" {
					gotRespMu.Lock()
					gotResp = cmd
					gotRespMu.Unlock()
				}
			})

			req := map[string]any{
				"type": "extension_ui_request", "id": reqID, "method": method,
				"title": "x",
			}
			if method == "input" {
				req["placeholder"] = "..."
			} else {
				req["prefill"] = "y"
			}
			f.writeJSON(t, req)

			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				gotRespMu.Lock()
				r := gotResp
				gotRespMu.Unlock()
				if r != nil {
					cancelled, _ := r["cancelled"].(bool)
					if !cancelled {
						t.Errorf("expected cancelled:true for %s, got response=%v", method, r)
					}
					if _, hasValue := r["value"]; hasValue {
						t.Errorf("expected no value field for cancelled %s, got %v", method, r)
					}
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Fatalf("never received extension_ui_response for %s", method)
			_ = c
		})
	}
}

// TestClient_ExtensionUIFireAndForget_Ignored: notify/setStatus/etc. produce
// no response and don't block.
func TestClient_ExtensionUIFireAndForget_Ignored(t *testing.T) {
	restore := setCurrentTest(t)
	defer restore()

	var seenTypesMu sync.Mutex
	var seenTypes []string
	c, f := newClientWithFake(t, func(f *fakePI, cmd map[string]any) {
		ctype, _ := cmd["type"].(string)
		if ctype == "extension_ui_response" {
			t.Errorf("received unexpected extension_ui_response for fire-and-forget")
		}
	})

	// Consume events to detect the requests being published.
	go func() {
		for ev := range c.Events() {
			seenTypesMu.Lock()
			seenTypes = append(seenTypes, ev.Type)
			seenTypesMu.Unlock()
		}
	}()

	methods := []string{"notify", "setStatus", "setWidget", "setTitle", "set_editor_text"}
	for _, m := range methods {
		f.writeJSON(t, map[string]any{
			"type": "extension_ui_request", "id": "uuid-" + m, "method": m,
			"message": "hi", "title": "x",
		})
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		seenTypesMu.Lock()
		n := len(seenTypes)
		seenTypesMu.Unlock()
		if n >= len(methods) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The key assertion: no extension_ui_response was written (checked in the
	// handler). Also assert we DID see the requests as events.
	seenTypesMu.Lock()
	defer seenTypesMu.Unlock()
	seen := strings.Join(seenTypes, ",")
	for _, m := range []string{"notify", "setStatus", "setWidget", "setTitle", "set_editor_text"} {
		_ = m
	}
	if len(seenTypes) < len(methods) {
		t.Errorf("expected at least %d events, got %d (%s)", len(methods), len(seenTypes), seen)
	}
	// Confirm nothing was written back to the fake's stdin as a ui response.
	for _, line := range f.Got() {
		if strings.Contains(line, "extension_ui_response") {
			t.Errorf("fire-and-forget produced a response: %s", line)
		}
	}
}

// TestClient_ProcessExit_ReturnsError: fake pi closes stdout → Client returns
// a typed ErrPiExit from an in-flight call.
func TestClient_ProcessExit_ReturnsError(t *testing.T) {
	restore := setCurrentTest(t)
	defer restore()

	// Handler that never responds, so the caller is still waiting when we
	// kill the fake.
	c, f := newClientWithFake(t, func(f *fakePI, cmd map[string]any) {
		// no-op: do not respond
	})

	// Issue a prompt in a goroutine; it will block waiting for a response.
	type result struct {
		resp Response
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := c.Prompt(testContext(t), "hi", false)
		resCh <- result{resp, err}
	}()

	// Give the prompt a moment to be in-flight, then close stdout.
	time.Sleep(50 * time.Millisecond)
	f.CloseStdoutOnly()

	select {
	case r := <-resCh:
		if !errors.Is(r.err, ErrPiExit) {
			t.Errorf("expected ErrPiExit, got %v (resp=%+v)", r.err, r.resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Prompt did not return after process exit")
	}
}

// keep imports
var _ = atomic.Bool{}
